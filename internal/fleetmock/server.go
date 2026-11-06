// Package fleetmock implements a mock fleet system for testing.
// It satisfies the fleet interface contract:
//
//	POST /v1/dispatch          → 202 (missionId, robotId) | 409 busy | 422 unknown dest
//	GET  /v1/missions/{id}     → queued | en_route | arrived | failed
//	GET  /v1/health            → 200
//
// A web control page at /control exposes toggles for failure modes.
package fleetmock

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// validDestinations lists all known destinations the fleet accepts.
var validDestinations = func() map[string]bool {
	m := make(map[string]bool)
	for _, prefix := range []string{"A", "B"} {
		for i := 1; i <= 10; i++ {
			m[fmt.Sprintf("%s%d", prefix, i)] = true
		}
	}
	return m
}()

// MissionStatus represents the lifecycle state of a fleet mission.
type MissionStatus string

const (
	MissionQueued   MissionStatus = "queued"
	MissionEnRoute  MissionStatus = "en_route"
	MissionArrived  MissionStatus = "arrived"
	MissionFailed   MissionStatus = "failed"
)

// Mission holds the state of a dispatched robot mission.
type Mission struct {
	MissionID   string        `json:"missionId"`
	RobotID     string        `json:"robotId"`
	RequestID   string        `json:"requestId"`
	Destination string        `json:"destination"`
	Status      MissionStatus `json:"status"`
	CreatedAt   time.Time     `json:"createdAt"`
}

// Controls holds the failure-mode toggles for the fleet mock.
type Controls struct {
	SlowReplies       bool `json:"slowReplies"`
	LostAfterDispatch bool `json:"lostAfterDispatch"`
	Unavailable       bool `json:"unavailable"`
	AlwaysBusy        bool `json:"alwaysBusy"`
}

// Server is the fleet mock HTTP server.
type Server struct {
	mu         sync.RWMutex
	missions   map[string]*Mission  // missionId → Mission
	byRequest  map[string]*Mission  // requestId → Mission (idempotency index)
	controls   Controls
	robotCount int
}

// NewServer creates and returns a new fleet mock server.
func NewServer() *Server {
	s := &Server{
		missions:   make(map[string]*Mission),
		byRequest:  make(map[string]*Mission),
		robotCount: 5,
	}
	return s
}

// Routes builds and returns the HTTP mux for the fleet mock.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/dispatch", s.handleDispatch)
	mux.HandleFunc("GET /v1/missions/{missionId}", s.handleGetMission)
	mux.HandleFunc("GET /control", s.handleControlPage)
	mux.HandleFunc("POST /control", s.handleControlUpdate)
	return corsMiddleware(mux)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleHealth returns 200 OK when the fleet is not in unavailable mode.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	unavailable := s.controls.Unavailable
	s.mu.RUnlock()

	if unavailable {
		http.Error(w, "fleet unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// DispatchRequest is the JSON body for POST /v1/dispatch.
type DispatchRequest struct {
	RequestID   string `json:"requestId"`
	Destination string `json:"destination"`
}

// handleDispatch processes a robot dispatch request.
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ctrl := s.controls
	s.mu.Unlock()

	// Simulate slow replies
	if ctrl.SlowReplies {
		time.Sleep(5 * time.Second)
	}

	// Simulate fleet unavailable
	if ctrl.Unavailable {
		http.Error(w, `{"error":"fleet unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	var req DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	// Validate destination
	if !validDestinations[req.Destination] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown destination"})
		return
	}

	s.mu.Lock()

	dropConnection := func() {
		s.mu.Unlock()
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}

	// Idempotency: return existing mission for the same requestId
	if existing, ok := s.byRequest[req.RequestID]; ok {
		log.Printf("fleet: idempotent dispatch for requestId=%s → missionId=%s", req.RequestID, existing.MissionID)
		// Simulate lost reply: mission exists but we don't respond
		if ctrl.LostAfterDispatch {
			dropConnection()
			return
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"missionId": existing.MissionID,
			"robotId":   existing.RobotID,
		})
		return
	}

	// Simulate always busy
	if ctrl.AlwaysBusy {
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]int{"retryAfterSeconds": 10})
		return
	}

	// Create new mission
	missionID := fmt.Sprintf("m-%d", time.Now().UnixNano())
	robotID := fmt.Sprintf("R%d", rand.Intn(s.robotCount)+1)

	mission := &Mission{
		MissionID:   missionID,
		RobotID:     robotID,
		RequestID:   req.RequestID,
		Destination: req.Destination,
		Status:      MissionQueued,
		CreatedAt:   time.Now(),
	}

	s.missions[missionID] = mission
	s.byRequest[req.RequestID] = mission

	// Advance mission through lifecycle asynchronously (~60 seconds to arrive)
	go s.advanceMission(missionID)

	// Simulate lost reply after dispatch: mission created but caller gets no response
	if ctrl.LostAfterDispatch {
		log.Printf("fleet: dispatch succeeded missionId=%s but reply is being dropped (lost-after-dispatch)", missionID)
		dropConnection()
		return
	}
	s.mu.Unlock()

	log.Printf("fleet: dispatched missionId=%s robotId=%s dest=%s", missionID, robotID, req.Destination)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"missionId": missionID,
		"robotId":   robotID,
	})
}

// advanceMission simulates a mission progressing from queued → en_route → arrived.
func (s *Server) advanceMission(missionID string) {
	// queued → en_route after ~5 seconds
	time.Sleep(5 * time.Second)
	s.mu.Lock()
	if m, ok := s.missions[missionID]; ok {
		m.Status = MissionEnRoute
		log.Printf("fleet: mission %s → en_route", missionID)
	}
	s.mu.Unlock()

	// en_route → arrived after ~55 more seconds (total ~60s)
	time.Sleep(55 * time.Second)
	s.mu.Lock()
	if m, ok := s.missions[missionID]; ok {
		m.Status = MissionArrived
		log.Printf("fleet: mission %s → arrived", missionID)
	}
	s.mu.Unlock()
}

// handleGetMission returns the current state of a mission.
func (s *Server) handleGetMission(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ctrl := s.controls
	s.mu.RUnlock()

	if ctrl.Unavailable {
		http.Error(w, `{"error":"fleet unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if ctrl.SlowReplies {
		time.Sleep(3 * time.Second)
	}

	missionID := r.PathValue("missionId")

	s.mu.RLock()
	mission, ok := s.missions[missionID]
	s.mu.RUnlock()

	if !ok {
		http.Error(w, `{"error":"mission not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"missionId": mission.MissionID,
		"status":    string(mission.Status),
		"robotId":   mission.RobotID,
	})
}

// handleControlUpdate processes form submissions from the control page.
func (s *Server) handleControlUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.controls.SlowReplies = r.FormValue("slowReplies") == "on"
	s.controls.LostAfterDispatch = r.FormValue("lostAfterDispatch") == "on"
	s.controls.Unavailable = r.FormValue("unavailable") == "on"
	s.controls.AlwaysBusy = r.FormValue("alwaysBusy") == "on"
	s.mu.Unlock()

	http.Redirect(w, r, "/control", http.StatusSeeOther)
}

var controlTmpl = template.Must(template.New("control").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Fleet Mock — Control Panel</title>
<style>
  *{box-sizing:border-box;margin:0;padding:0}
  body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;padding:2rem}
  h1{font-size:1.5rem;margin-bottom:0.25rem;color:#f8fafc}
  .subtitle{color:#94a3b8;margin-bottom:2rem;font-size:0.875rem}
  .card{background:#1e293b;border:1px solid #334155;border-radius:0.75rem;padding:1.5rem;margin-bottom:1.5rem}
  h2{font-size:1rem;color:#94a3b8;text-transform:uppercase;letter-spacing:0.05em;margin-bottom:1rem}
  .toggle-row{display:flex;align-items:center;justify-content:space-between;padding:0.75rem 0;border-bottom:1px solid #334155}
  .toggle-row:last-child{border-bottom:none}
  .toggle-label{font-size:0.9rem}
  .toggle-desc{font-size:0.75rem;color:#64748b;margin-top:0.2rem}
  .toggle{position:relative;width:44px;height:24px}
  .toggle input{opacity:0;width:0;height:0}
  .slider{position:absolute;cursor:pointer;inset:0;background:#334155;border-radius:9999px;transition:0.2s}
  .slider:before{content:"";position:absolute;width:16px;height:16px;left:4px;top:4px;background:#94a3b8;border-radius:50%;transition:0.2s}
  input:checked + .slider{background:#ef4444}
  input:checked + .slider:before{transform:translateX(20px);background:#fff}
  .btn{background:#3b82f6;color:#fff;border:none;padding:0.6rem 1.5rem;border-radius:0.5rem;cursor:pointer;font-size:0.9rem;margin-top:1rem}
  .btn:hover{background:#2563eb}
  table{width:100%;border-collapse:collapse;font-size:0.8rem}
  th{text-align:left;color:#64748b;padding:0.5rem;border-bottom:1px solid #334155}
  td{padding:0.5rem;border-bottom:1px solid #1e293b}
  .badge{display:inline-block;padding:0.15rem 0.5rem;border-radius:9999px;font-size:0.7rem;font-weight:600}
  .queued{background:#1e3a5f;color:#60a5fa}
  .en_route{background:#1c3a2e;color:#34d399}
  .arrived{background:#14532d;color:#4ade80}
  .failed{background:#450a0a;color:#f87171}
</style>
</head>
<body>
<h1>🤖 Fleet Mock — Control Panel</h1>
<p class="subtitle">Use these switches to simulate failure conditions. Changes take effect immediately.</p>

<div class="card">
  <h2>Failure Mode Toggles</h2>
  <form method="POST" action="/control">
    <div class="toggle-row">
      <div>
        <div class="toggle-label">Slow Replies</div>
        <div class="toggle-desc">Adds a 5s delay to dispatch, 3s to mission queries</div>
      </div>
      <label class="toggle">
        <input type="checkbox" name="slowReplies" {{if .Controls.SlowReplies}}checked{{end}}>
        <span class="slider"></span>
      </label>
    </div>
    <div class="toggle-row">
      <div>
        <div class="toggle-label">Reply Lost After Dispatch</div>
        <div class="toggle-desc">Mission is created but caller receives no HTTP response (simulates network drop)</div>
      </div>
      <label class="toggle">
        <input type="checkbox" name="lostAfterDispatch" {{if .Controls.LostAfterDispatch}}checked{{end}}>
        <span class="slider"></span>
      </label>
    </div>
    <div class="toggle-row">
      <div>
        <div class="toggle-label">Fleet Unavailable</div>
        <div class="toggle-desc">All endpoints return 503 Service Unavailable</div>
      </div>
      <label class="toggle">
        <input type="checkbox" name="unavailable" {{if .Controls.Unavailable}}checked{{end}}>
        <span class="slider"></span>
      </label>
    </div>
    <div class="toggle-row">
      <div>
        <div class="toggle-label">Always Busy</div>
        <div class="toggle-desc">All dispatch requests return 409 with retryAfterSeconds=10</div>
      </div>
      <label class="toggle">
        <input type="checkbox" name="alwaysBusy" {{if .Controls.AlwaysBusy}}checked{{end}}>
        <span class="slider"></span>
      </label>
    </div>
    <button class="btn" type="submit">Apply</button>
  </form>
</div>

<div class="card">
  <h2>Missions ({{len .Missions}})</h2>
  {{if .Missions}}
  <table>
    <thead><tr><th>Mission ID</th><th>Request ID</th><th>Robot</th><th>Destination</th><th>Status</th><th>Created</th></tr></thead>
    <tbody>
    {{range .Missions}}
    <tr>
      <td>{{.MissionID}}</td>
      <td style="font-size:0.7rem;color:#64748b">{{.RequestID}}</td>
      <td>{{.RobotID}}</td>
      <td>{{.Destination}}</td>
      <td><span class="badge {{.Status}}">{{.Status}}</span></td>
      <td>{{.CreatedAt.Format "15:04:05"}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <p style="color:#64748b;font-size:0.875rem">No missions yet.</p>
  {{end}}
</div>
<script>
// Auto-refresh the mission list every 5 seconds
setTimeout(() => location.reload(), 5000);
</script>
</body>
</html>`))

func (s *Server) handleControlPage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ctrl := s.controls
	missions := make([]*Mission, 0, len(s.missions))
	for _, m := range s.missions {
		missions = append(missions, m)
	}
	s.mu.RUnlock()

	// Sort missions newest first (simple bubble sort for small N)
	for i := 0; i < len(missions); i++ {
		for j := i + 1; j < len(missions); j++ {
			if missions[j].CreatedAt.After(missions[i].CreatedAt) {
				missions[i], missions[j] = missions[j], missions[i]
			}
		}
	}

	data := struct {
		Controls Controls
		Missions []*Mission
	}{ctrl, missions}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := controlTmpl.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

// GetControls returns a copy of the current control state (used in tests).
func (s *Server) GetControls() Controls {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.controls
}

// SetControls sets the control state directly (used in tests).
func (s *Server) SetControls(c Controls) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controls = c
}

// MissionCount returns the number of missions created (used in tests).
func (s *Server) MissionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.missions)
}


