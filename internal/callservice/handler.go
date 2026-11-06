package callservice

import (
	"context"
	"encoding/json"
	"fmt"
	htmltmpl "html/template"
	"log"
	"net/http"
	"os"
	"strings"
	texttmpl "text/template"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// StationConfig represents one station definition from stations.yaml.
type StationConfig struct {
	ID          string `yaml:"id"`
	Destination string `yaml:"destination"`
	Token       string `yaml:"token"`
}

// Config is the top-level config file structure.
type Config struct {
	Stations []StationConfig `yaml:"stations"`
}

// Server is the HTTP server for the call service.
type Server struct {
	store    *Store
	broker   *SSEBroker
	engine   *Engine
	stations map[string]StationConfig // stationId → config
	cancel   context.CancelFunc
}

// NewServer creates a Server and starts the background engine.
func NewServer(dsn, fleetURL, stationsFile string) (*Server, error) {
	store, err := NewStore(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	cfg, err := loadConfig(stationsFile)
	if err != nil {
		return nil, fmt.Errorf("load stations config: %w", err)
	}

	stationMap := make(map[string]StationConfig, len(cfg.Stations))
	for _, st := range cfg.Stations {
		stationMap[st.ID] = st
	}

	broker := NewSSEBroker()
	engine := NewEngine(store, broker, fleetURL)

	ctx, cancel := context.WithCancel(context.Background())
	go engine.Run(ctx)

	return &Server{
		store:    store,
		broker:   broker,
		engine:   engine,
		stations: stationMap,
		cancel:   cancel,
	}, nil
}

// Close shuts down the server.
func (s *Server) Close() {
	s.cancel()
	_ = s.store.Close()
}

// Routes returns the HTTP mux for the call service.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Station API
	mux.HandleFunc("POST /api/call/{stationId}", s.authMiddleware(s.handleCall))
	mux.HandleFunc("GET /api/call/{stationId}", s.authMiddleware(s.handleGetCall))
	mux.HandleFunc("GET /api/events/{stationId}", s.authMiddleware(s.handleSSE))

	// Call log API
	mux.HandleFunc("GET /api/calls", s.handleListCalls)

	// Pages
	mux.HandleFunc("GET /station/{stationId}", s.handleStationPage)
	mux.HandleFunc("GET /calls", s.handleCallLogPage)

	// Health
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	return corsMiddleware(mux)
}

// authMiddleware validates the Bearer token for station endpoints.
// Accepts token via Authorization header OR ?token= query param (required for EventSource).
func (s *Server) authMiddleware(next func(http.ResponseWriter, *http.Request, StationConfig)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stationID := r.PathValue("stationId")
		cfg, ok := s.stations[stationID]
		if !ok {
			http.Error(w, `{"error":"unknown station"}`, http.StatusNotFound)
			return
		}

		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token") // fallback for EventSource (SSE)
		}
		if token != cfg.Token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		next(w, r, cfg)
	}
}

// handleCall processes a button press: POST /api/call/{stationId}
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request, cfg StationConfig) {
	ctx := r.Context()

	// Check for existing open call (service layer, belt)
	existing, err := s.store.GetOpenCall(ctx, cfg.ID)
	if err != nil {
		log.Printf("handler: get open call: %v", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if existing != nil {
		// Duplicate press — return existing call, 200 OK (not 409; UX shows current state)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(callResponse(existing, "call already active"))
		return
	}

	// Create new call
	requestID := uuid.New().String()
	call, err := s.store.CreateCall(ctx, cfg.ID, cfg.Destination, requestID)
	if err != nil {
		// Could be a unique constraint violation (race condition caught at DB layer)
		if strings.Contains(err.Error(), "one_open_call_per_station") ||
			strings.Contains(err.Error(), "unique") {
			existing, _ = s.store.GetOpenCall(ctx, cfg.ID)
			if existing != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(callResponse(existing, "call already active"))
				return
			}
		}
		log.Printf("handler: create call: %v", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("handler: new call callId=%s stationId=%s requestId=%s", call.ID, cfg.ID, requestID)

	// Kick off dispatch immediately (non-blocking)
	s.engine.DispatchNewCall(call)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(callResponse(call, ""))
}

// handleGetCall returns the current state of a station's most recent call.
func (s *Server) handleGetCall(w http.ResponseWriter, r *http.Request, cfg StationConfig) {
	ctx := r.Context()

	call, err := s.store.GetLatestCallForStation(ctx, cfg.ID)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if call == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "idle"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(callResponse(call, ""))
}

// handleSSE streams real-time call status updates to the station page.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request, cfg StationConfig) {
	ctx := r.Context()

	current, _ := s.store.GetLatestCallForStation(ctx, cfg.ID)
	s.broker.ServeSSE(w, r, cfg.ID, current)
}

// handleListCalls returns all calls for the call log page.
func (s *Server) handleListCalls(w http.ResponseWriter, r *http.Request) {
	calls, err := s.store.ListAllCalls(r.Context())
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	type callWithEvents struct {
		ID          string       `json:"id"`
		StationID   string       `json:"stationId"`
		Destination string       `json:"destination"`
		Status      CallStatus   `json:"status"`
		RobotID     string       `json:"robotId,omitempty"`
		CreatedAt   time.Time    `json:"createdAt"`
		UpdatedAt   time.Time    `json:"updatedAt"`
		Events      []*CallEvent `json:"events"`
	}

	result := make([]callWithEvents, 0, len(calls))
	for _, c := range calls {
		events, _ := s.store.ListCallEvents(r.Context(), c.ID)
		item := callWithEvents{
			ID:          c.ID,
			StationID:   c.StationID,
			Destination: c.Destination,
			Status:      c.Status,
			CreatedAt:   c.CreatedAt,
			UpdatedAt:   c.UpdatedAt,
			Events:      events,
		}
		if c.RobotID.Valid {
			item.RobotID = c.RobotID.String
		}
		result = append(result, item)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// callResponse builds the JSON response for a call.
func callResponse(c *Call, message string) map[string]interface{} {
	r := map[string]interface{}{
		"callId":      c.ID,
		"stationId":   c.StationID,
		"destination": c.Destination,
		"status":      c.Status,
		"requestId":   c.RequestID,
		"createdAt":   c.CreatedAt,
		"updatedAt":   c.UpdatedAt,
	}
	if c.MissionID.Valid {
		r["missionId"] = c.MissionID.String
	}
	if c.RobotID.Valid {
		r["robotId"] = c.RobotID.String
	}
	if message != "" {
		r["message"] = message
	}
	return r
}

// loadConfig reads and parses the stations YAML config file.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
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

// --- Page handlers ---

var stationTmpl = texttmpl.Must(texttmpl.New("station").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Station {{.StationID}} — Call Station</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;min-height:100vh;display:flex;align-items:center;justify-content:center;flex-direction:column;gap:2rem;padding:2rem}
.station-id{font-size:0.875rem;color:#64748b;text-transform:uppercase;letter-spacing:0.1em}
h1{font-size:2rem;color:#f8fafc}
.call-btn{width:160px;height:160px;border-radius:50%;border:4px solid #3b82f6;background:#1e3a5f;color:#60a5fa;font-size:1.1rem;font-weight:700;cursor:pointer;transition:all 0.15s;display:flex;align-items:center;justify-content:center;text-align:center;user-select:none;box-shadow:0 0 0 0 rgba(59,130,246,0.4)}
.call-btn:hover:not(:disabled){background:#1d4ed8;border-color:#60a5fa;box-shadow:0 0 0 12px rgba(59,130,246,0.15)}
.call-btn:active:not(:disabled){transform:scale(0.96)}
.call-btn:disabled{opacity:0.5;cursor:not-allowed;border-color:#475569}
.status-box{padding:1rem 2rem;border-radius:0.75rem;text-align:center;min-width:280px;border:1px solid;transition:all 0.3s}
.status-idle{background:#1e293b;border-color:#334155;color:#94a3b8}
.status-pending,.status-dispatching{background:#1c2a3a;border-color:#2563eb;color:#93c5fd;animation:pulse 2s infinite}
.status-en_route{background:#1c3a2e;border-color:#059669;color:#34d399}
.status-arrived{background:#14532d;border-color:#16a34a;color:#4ade80}
.status-failed{background:#450a0a;border-color:#dc2626;color:#f87171}
.status-label{font-size:1rem;font-weight:600;text-transform:uppercase;letter-spacing:0.05em}
.status-detail{font-size:0.8rem;margin-top:0.4rem;opacity:0.8}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:0.7}}
.net-loss{display:flex;align-items:center;gap:0.75rem;font-size:0.875rem;color:#94a3b8;margin-top:0.5rem}
.toggle{position:relative;width:44px;height:24px}
.toggle input{opacity:0;width:0;height:0}
.slider{position:absolute;cursor:pointer;inset:0;background:#334155;border-radius:9999px;transition:0.2s}
.slider:before{content:"";position:absolute;width:16px;height:16px;left:4px;top:4px;background:#94a3b8;border-radius:50%;transition:0.2s}
input:checked+.slider{background:#ef4444}
input:checked+.slider:before{transform:translateX(20px);background:#fff}
.conn-dot{width:8px;height:8px;border-radius:50%;display:inline-block;margin-right:4px}
.conn-ok{background:#22c55e}
.conn-err{background:#ef4444}
</style>
</head>
<body>
<div class="station-id">Warehouse Station</div>
<h1>{{.StationID}}</h1>
<div>
  <span class="conn-dot conn-ok" id="conn-dot"></span>
  <span id="conn-status" style="font-size:0.75rem;color:#64748b">Connected</span>
</div>

<button class="call-btn" id="call-btn" onclick="pressCall()">
  CALL<br>ROBOT
</button>

<div class="status-box status-idle" id="status-box">
  <div class="status-label" id="status-label">Idle</div>
  <div class="status-detail" id="status-detail">Press the button to call a robot</div>
</div>

<div class="net-loss">
  <label class="toggle">
    <input type="checkbox" id="netloss-toggle" onchange="toggleNetLoss(this.checked)">
    <span class="slider"></span>
  </label>
  Simulate network loss
</div>

<script>
const STATION_ID = "{{.StationID}}";
const TOKEN = "{{.Token}}";
const API = '';

let evtSource = null;
let netLoss = false;

const statusClasses = {
  idle: 'status-idle',
  pending: 'status-pending',
  dispatching: 'status-dispatching',
  en_route: 'status-en_route',
  arrived: 'status-arrived',
  failed: 'status-failed',
};

const statusLabels = {
  idle: 'Idle',
  pending: 'Pending',
  dispatching: 'Dispatching…',
  en_route: 'Robot en Route',
  arrived: '✓ Arrived',
  failed: '✗ Failed',
};

const statusDetails = {
  idle: 'Press the button to call a robot',
  pending: 'Request received…',
  dispatching: 'Finding available robot…',
  en_route: 'Robot is on the way',
  arrived: 'Robot has arrived at this station',
  failed: 'Could not complete the call',
};

function setStatus(status, detail, robotId) {
  const box = document.getElementById('status-box');
  const label = document.getElementById('status-label');
  const det = document.getElementById('status-detail');
  const btn = document.getElementById('call-btn');

  box.className = 'status-box ' + (statusClasses[status] || 'status-idle');
  label.textContent = statusLabels[status] || status;
  let d = detail || statusDetails[status] || '';
  if (robotId) d += ' · Robot ' + robotId;
  det.textContent = d;

  const isOpen = ['pending','dispatching','en_route'].includes(status);
  btn.disabled = isOpen;
}

function connect() {
  if (netLoss) return;
  if (evtSource) { evtSource.close(); evtSource = null; }

  setConnStatus(false, 'Connecting…');
  // EventSource does not support custom headers; pass token as query param instead.
  evtSource = new EventSource(API + '/api/events/' + STATION_ID + '?token=' + encodeURIComponent(TOKEN));

  evtSource.onopen = () => setConnStatus(true, 'Connected');
  evtSource.onerror = () => {
    setConnStatus(false, 'Reconnecting…');
    if (evtSource) { evtSource.close(); evtSource = null; }
    if (!netLoss) setTimeout(connect, 3000);
  };
  evtSource.onmessage = (e) => {
    const data = JSON.parse(e.data);
    setStatus(data.status, null, data.robotId);
  };

  // Fetch current state immediately on (re)connect — station holds no local state
  fetch(API + '/api/call/' + STATION_ID + '?token=' + encodeURIComponent(TOKEN))
    .then(r => r.json())
    .then(d => setStatus(d.status || 'idle', null, d.robotId))
    .catch(() => {});
}

function setConnStatus(ok, msg) {
  document.getElementById('conn-dot').className = 'conn-dot ' + (ok ? 'conn-ok' : 'conn-err');
  document.getElementById('conn-status').textContent = msg;
}

function pressCall() {
  if (netLoss) return;
  fetch(API + '/api/call/' + STATION_ID, {
    method: 'POST',
    headers: { Authorization: 'Bearer ' + TOKEN, 'Content-Type': 'application/json' },
    body: '{}'
  }).then(r => r.json()).then(d => {
    setStatus(d.status, d.message, d.robotId);
  }).catch(err => {
    console.error('call error', err);
  });
}

function toggleNetLoss(on) {
  netLoss = on;
  if (on) {
    if (evtSource) evtSource.close();
    evtSource = null;
    setConnStatus(false, 'Network loss simulated');
  } else {
    connect();
  }
}

connect();
</script>
</body>
</html>`))

// handleStationPage serves the station UI page.
func (s *Server) handleStationPage(w http.ResponseWriter, r *http.Request) {
	stationID := r.PathValue("stationId")
	cfg, ok := s.stations[stationID]
	if !ok {
		http.Error(w, "unknown station", http.StatusNotFound)
		return
	}

	data := struct {
		StationID string
		Token     string
	}{cfg.ID, cfg.Token}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := stationTmpl.Execute(w, data); err != nil {
		log.Printf("station page template error: %v", err)
	}
}

var callLogTmpl = htmltmpl.Must(htmltmpl.New("calllog").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Call Log — Call Station</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;padding:2rem}
h1{font-size:1.5rem;margin-bottom:0.25rem;color:#f8fafc}
.subtitle{color:#94a3b8;margin-bottom:2rem;font-size:0.875rem}
.card{background:#1e293b;border:1px solid #334155;border-radius:0.75rem;padding:1.5rem;margin-bottom:1rem}
.call-header{display:flex;align-items:center;gap:1rem;margin-bottom:0.75rem;flex-wrap:wrap}
.station-tag{font-weight:700;color:#f8fafc;font-size:1rem}
.dest-tag{color:#94a3b8;font-size:0.875rem}
.robot-tag{color:#60a5fa;font-size:0.875rem}
.badge{display:inline-block;padding:0.15rem 0.5rem;border-radius:9999px;font-size:0.7rem;font-weight:600}
.pending,.dispatching{background:#1c2a3a;color:#93c5fd}
.en_route{background:#1c3a2e;color:#34d399}
.arrived{background:#14532d;color:#4ade80}
.failed{background:#450a0a;color:#f87171}
.timeline{border-left:2px solid #334155;padding-left:1rem;margin-left:0.5rem}
.event{position:relative;padding:0.3rem 0;font-size:0.8rem}
.event::before{content:"";position:absolute;left:-1.35rem;top:0.5rem;width:8px;height:8px;border-radius:50%;background:#334155}
.event-status{color:#94a3b8;text-transform:uppercase;font-size:0.7rem;letter-spacing:0.05em}
.event-note{color:#cbd5e1}
.event-ts{color:#475569;font-size:0.7rem}
.refresh-btn{background:#1e293b;border:1px solid #334155;color:#94a3b8;padding:0.4rem 1rem;border-radius:0.5rem;cursor:pointer;font-size:0.8rem;margin-bottom:1rem}
.refresh-btn:hover{border-color:#60a5fa;color:#60a5fa}
.empty{color:#64748b;text-align:center;padding:3rem}
</style>
</head>
<body>
<h1>📋 Call Log</h1>
<p class="subtitle">Read-only. Auto-refreshes every 10 seconds.</p>
<button class="refresh-btn" onclick="loadCalls()">↻ Refresh now</button>
<div id="calls-container"><p class="empty">Loading…</p></div>

<script>
function fmt(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  return d.toLocaleDateString() + ' ' + d.toLocaleTimeString();
}
function statusBadge(s) {
  if (!s) return '';
  return '<span class="badge '+s+'">'+s.replace(/_/g,' ')+'</span>';
}
function loadCalls() {
  fetch('/api/calls').then(r=>r.json()).then(calls=>{
    const c = document.getElementById('calls-container');
    if (!calls || calls.length===0){c.innerHTML='<p class="empty">No calls yet.</p>';return;}
    c.innerHTML = calls.map(call=>{
      const events = (call.events||[]).map(e=>{
        const status = e.status || e.Status || '';
        const noteText = e.note || e.Note || '';
        const note = noteText ? '<span class="event-note">— '+noteText+'</span>' : '';
        const ts = e.ts || e.TS;
        return '<div class="event"><div><span class="event-status">'+status+'</span> '+note+'</div><div class="event-ts">'+fmt(ts)+'</div></div>';
      }).join('');
      return '<div class="card">'+
        '<div class="call-header">'+
          '<span class="station-tag">'+call.stationId+'</span>'+
          '<span class="dest-tag">→ '+call.destination+'</span>'+
          (call.robotId?'<span class="robot-tag">🤖 '+call.robotId+'</span>':'')+
          statusBadge(call.status)+
          '<span style="color:#475569;font-size:0.75rem;margin-left:auto">'+fmt(call.createdAt)+'</span>'+
        '</div>'+
        '<div class="timeline">'+events+'</div>'+
      '</div>';
    }).join('');
  }).catch(()=>{document.getElementById('calls-container').innerHTML='<p class="empty">Error loading calls.</p>';});
}
loadCalls();
setInterval(loadCalls, 10000);
</script>
</body>
</html>`))

// handleCallLogPage serves the call log UI page.
func (s *Server) handleCallLogPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := callLogTmpl.Execute(w, nil); err != nil {
		log.Printf("calllog template error: %v", err)
	}
}
