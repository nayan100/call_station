package callservice

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// SSEEvent is a typed event pushed over the SSE stream.
type SSEEvent struct {
	CallID    string     `json:"callId"`
	StationID string     `json:"stationId"`
	Status    CallStatus `json:"status"`
	MissionID string     `json:"missionId,omitempty"`
	RobotID   string     `json:"robotId,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// subscriber holds a channel for one SSE client connection.
type subscriber struct {
	stationID string
	ch        chan SSEEvent
}

// SSEBroker manages all active SSE client connections and fans out events.
type SSEBroker struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

// NewSSEBroker creates a new SSE broker.
func NewSSEBroker() *SSEBroker {
	return &SSEBroker{subs: make(map[*subscriber]struct{})}
}

// Publish sends an event to all subscribers listening for the given stationID.
func (b *SSEBroker) Publish(evt SSEEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs {
		if sub.stationID == evt.StationID {
			select {
			case sub.ch <- evt:
			default:
				// Drop if the subscriber's buffer is full; they'll recover on reconnect.
			}
		}
	}
}

// subscribe registers a new subscriber and returns it.
func (b *SSEBroker) subscribe(stationID string) *subscriber {
	sub := &subscriber{
		stationID: stationID,
		ch:        make(chan SSEEvent, 8),
	}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	return sub
}

// unsubscribe removes a subscriber.
func (b *SSEBroker) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	delete(b.subs, sub)
	b.mu.Unlock()
}

// ServeSSE is the HTTP handler for GET /api/events/{stationId}.
// It streams SSE events to the client until the connection closes.
func (b *SSEBroker) ServeSSE(w http.ResponseWriter, r *http.Request, stationID string, currentState *Call) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	sub := b.subscribe(stationID)
	defer b.unsubscribe(sub)

	// Send current state immediately so reconnecting clients get instant truth
	if currentState != nil {
		evt := callToSSEEvent(currentState)
		if err := writeSSEEvent(w, evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Keep-alive ticker — prevents proxy timeouts on idle connections
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-sub.ch:
			if err := writeSSEEvent(w, evt); err != nil {
				log.Printf("sse write error station=%s: %v", stationID, err)
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// SSE comment as keep-alive ping
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, evt SSEEvent) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func callToSSEEvent(c *Call) SSEEvent {
	evt := SSEEvent{
		CallID:    c.ID,
		StationID: c.StationID,
		Status:    c.Status,
		UpdatedAt: c.UpdatedAt,
	}
	if c.MissionID.Valid {
		evt.MissionID = c.MissionID.String
	}
	if c.RobotID.Valid {
		evt.RobotID = c.RobotID.String
	}
	return evt
}
