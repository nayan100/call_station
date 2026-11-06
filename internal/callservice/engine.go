package callservice

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	dispatchTimeout = 10 * time.Minute
	enRouteTimeout  = 15 * time.Minute
	pollInterval    = 5 * time.Second

	// Backoff parameters for fleet unavailable / error
	backoffBase    = 2 * time.Second
	backoffMax     = 60 * time.Second
	backoffJitter  = 0.20 // ±20%
	dispatchHTTPTimeout = 8 * time.Second
)

// Engine manages the call lifecycle: dispatching, retrying, and mission polling.
type Engine struct {
	store    *Store
	broker   *SSEBroker
	fleetURL string
	client   *http.Client
}

// NewEngine creates a new call engine.
func NewEngine(store *Store, broker *SSEBroker, fleetURL string) *Engine {
	fleetURL = strings.TrimRight(fleetURL, "/")
	if !strings.HasPrefix(fleetURL, "http://") && !strings.HasPrefix(fleetURL, "https://") {
		if strings.Contains(fleetURL, ".") {
			fleetURL = "https://" + fleetURL
		} else {
			fleetURL = "http://" + fleetURL
		}
	}
	if u, err := url.Parse(fleetURL); err == nil && u.Port() == "" && !strings.Contains(u.Hostname(), ".") && u.Hostname() != "localhost" {
		// Default to internal port 8081 if only internal service name is provided
		u.Host = u.Hostname() + ":8081"
		fleetURL = u.String()
	}

	return &Engine{
		store:    store,
		broker:   broker,
		fleetURL: fleetURL,
		client:   &http.Client{Timeout: dispatchHTTPTimeout},
	}
}

// Run starts the background poller. It blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	log.Println("engine: starting background poller")
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("engine: poller stopped")
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// tick runs one poller cycle.
func (e *Engine) tick(ctx context.Context) {
	// 1. Timeout stale calls
	if err := e.store.TimeoutStaleCalls(ctx, dispatchTimeout, enRouteTimeout); err != nil {
		log.Printf("engine: timeout stale calls: %v", err)
	}

	// 2. Dispatch any pending/dispatching calls that are ready for retry
	dispatchable, err := e.store.GetActiveDispatchingCalls(ctx)
	if err != nil {
		log.Printf("engine: get dispatching calls: %v", err)
	}
	for _, call := range dispatchable {
		go e.dispatchCall(ctx, call)
	}

	// 3. Poll mission status for en_route calls
	enRoute, err := e.store.GetActiveMissionCalls(ctx)
	if err != nil {
		log.Printf("engine: get mission calls: %v", err)
	}
	for _, call := range enRoute {
		go e.pollMission(ctx, call)
	}
}

// DispatchNewCall is called when a new call is created (immediate first attempt).
func (e *Engine) DispatchNewCall(call *Call) {
	go e.dispatchCall(context.Background(), call)
}

// dispatchCall attempts to dispatch a call to the fleet.
func (e *Engine) dispatchCall(ctx context.Context, call *Call) {
	log.Printf("engine: dispatching callId=%s stationId=%s requestId=%s attempt=%d",
		call.ID, call.StationID, call.RequestID, call.RetryCount+1)

	// Mark as dispatching
	if call.Status == StatusPending {
		if err := e.store.UpdateCallStatus(ctx, call.ID, StatusDispatching, "", "", "dispatch attempt started"); err != nil {
			log.Printf("engine: update to dispatching: %v", err)
			return
		}
		call.Status = StatusDispatching
		e.broker.Publish(callToSSEEvent(call))
	}

	resp, err := e.postDispatch(ctx, call.RequestID, call.Destination)
	if err != nil {
		// Network error or timeout — could be reply lost or truly unavailable
		log.Printf("engine: dispatch HTTP error callId=%s: %v", call.ID, err)
		delay := e.backoffDelay(call.RetryCount)
		if retryErr := e.store.IncrementRetry(ctx, call.ID, time.Now().Add(delay)); retryErr != nil {
			log.Printf("engine: increment retry: %v", retryErr)
		}
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusAccepted:
		var body struct {
			MissionID string `json:"missionId"`
			RobotID   string `json:"robotId"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			log.Printf("engine: decode 202 body callId=%s: %v", call.ID, err)
			delay := e.backoffDelay(call.RetryCount)
			_ = e.store.IncrementRetry(ctx, call.ID, time.Now().Add(delay))
			return
		}
		log.Printf("engine: dispatch accepted callId=%s missionId=%s robotId=%s",
			call.ID, body.MissionID, body.RobotID)
		if err := e.store.UpdateCallStatus(ctx, call.ID, StatusEnRoute, body.MissionID, body.RobotID, "fleet accepted"); err != nil {
			log.Printf("engine: update to en_route: %v", err)
			return
		}
		call.Status = StatusEnRoute
		call.MissionID.String = body.MissionID
		call.MissionID.Valid = true
		call.RobotID.String = body.RobotID
		call.RobotID.Valid = true
		e.broker.Publish(callToSSEEvent(call))

	case http.StatusConflict: // 409 busy
		var body struct {
			RetryAfterSeconds int `json:"retryAfterSeconds"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		delay := time.Duration(body.RetryAfterSeconds) * time.Second
		if delay <= 0 {
			delay = 30 * time.Second
		}
		log.Printf("engine: fleet busy callId=%s retryAfter=%v", call.ID, delay)
		if err := e.store.IncrementRetry(ctx, call.ID, time.Now().Add(delay)); err != nil {
			log.Printf("engine: increment retry (busy): %v", err)
		}

	case http.StatusUnprocessableEntity: // 422 unknown destination
		log.Printf("engine: unknown destination callId=%s dest=%s — marking failed", call.ID, call.Destination)
		_ = e.store.UpdateCallStatus(ctx, call.ID, StatusFailed, "", "", "unknown destination")
		call.Status = StatusFailed
		e.broker.Publish(callToSSEEvent(call))

	default:
		// 503 or other transient error
		log.Printf("engine: fleet returned %d for callId=%s", resp.StatusCode, call.ID)
		delay := e.backoffDelay(call.RetryCount)
		_ = e.store.IncrementRetry(ctx, call.ID, time.Now().Add(delay))
	}
}

// pollMission checks the fleet for the current mission status.
func (e *Engine) pollMission(ctx context.Context, call *Call) {
	if !call.MissionID.Valid {
		return
	}

	url := fmt.Sprintf("%s/v1/missions/%s", e.fleetURL, call.MissionID.String)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := e.client.Do(req)
	if err != nil {
		log.Printf("engine: poll mission callId=%s: %v", call.ID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("engine: mission poll returned %d for callId=%s", resp.StatusCode, call.ID)
		return
	}

	var body struct {
		Status  string `json:"status"`
		RobotID string `json:"robotId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return
	}

	switch body.Status {
	case "arrived":
		log.Printf("engine: mission arrived callId=%s", call.ID)
		_ = e.store.UpdateCallStatus(ctx, call.ID, StatusArrived, "", body.RobotID, "robot arrived")
		call.Status = StatusArrived
		e.broker.Publish(callToSSEEvent(call))
	case "failed":
		log.Printf("engine: mission failed callId=%s", call.ID)
		_ = e.store.UpdateCallStatus(ctx, call.ID, StatusFailed, "", "", "mission failed")
		call.Status = StatusFailed
		e.broker.Publish(callToSSEEvent(call))
	}
	// queued / en_route: no state change needed, continue polling
}

// postDispatch sends a POST /v1/dispatch to the fleet.
func (e *Engine) postDispatch(ctx context.Context, requestID, destination string) (*http.Response, error) {
	body := fmt.Sprintf(`{"requestId":%q,"destination":%q}`, requestID, destination)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.fleetURL+"/v1/dispatch",
		strings.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return e.client.Do(req)
}

// backoffDelay computes the next retry delay with exponential backoff + jitter.
func (e *Engine) backoffDelay(retryCount int) time.Duration {
	base := float64(backoffBase)
	for i := 0; i < retryCount && base < float64(backoffMax); i++ {
		base *= 2
	}
	if base > float64(backoffMax) {
		base = float64(backoffMax)
	}
	// Add ±20% jitter
	jitter := base * backoffJitter * (rand.Float64()*2 - 1)
	d := time.Duration(base + jitter)
	if d < backoffBase {
		d = backoffBase
	}
	return d
}
