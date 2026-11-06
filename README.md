# Call Station

> Take-home assignment for Virya Autonomous Technologies — Systems Solutions Engineer

A system for warehouse operators to call robots to their station via a web interface. Robots are managed through a fleet dispatch API.

---

## Quick Start (Local)

**Single command:**
```bash
docker compose up --build
```

| Page | URL |
|------|-----|
| Station A1 | http://localhost:8080/station/A1 |
| Station B3 | http://localhost:8080/station/B3 |
| Call Log | http://localhost:8080/calls |
| Fleet Control | http://localhost:8081/control |

> **Note on cold start:** On Render.com's free tier, the first request after inactivity may take ~30 seconds while the service wakes up. This is a free-tier limitation, documented here per the assignment requirements.

---

## Running Tests (P5)

The two automated tests require a PostgreSQL instance:

```bash
# Start just the database
docker compose up db -d

# Run integration tests
TEST_DATABASE_URL="postgres://callstation:callstation@localhost:5432/callstation?sslmode=disable" \
  go test ./internal/callservice/tests/... -v -timeout 120s
```

**Test 1 — Double Press:** Verifies that pressing the button twice (even concurrently) creates exactly one open call per station. The second press returns the existing call, not a new one.

**Test 2 — Reply Lost After Dispatch:** Enables the fleet mock's "lost after dispatch" mode, presses the button, then disables the failure. Verifies the call service retries with the same `requestId` (fleet idempotency), eventually reaches `en_route`, and that only ONE fleet mission was created despite multiple dispatch attempts.

---

## Fleet Mock Control Page

Navigate to `http://localhost:8081/control` and use the toggles to simulate:

| Toggle | Effect |
|--------|--------|
| **Slow Replies** | Adds 5s delay to dispatch, 3s to mission queries |
| **Reply Lost After Dispatch** | Mission is created but HTTP response is dropped (simulates network drop) |
| **Fleet Unavailable** | All endpoints return 503 |
| **Always Busy** | Dispatch returns 409 with `retryAfterSeconds=10` |

The mission list auto-refreshes every 5 seconds showing all missions and their current status.

---

## Project Structure

```
Call_Station/
├── cmd/
│   ├── callservice/main.go     # Call Service entry point
│   └── fleetmock/main.go       # Fleet Mock entry point
├── internal/
│   ├── callservice/
│   │   ├── store.go            # PostgreSQL queries and schema
│   │   ├── sse.go              # SSE broker (fan-out to connected clients)
│   │   ├── engine.go           # Call lifecycle: dispatch, retry, mission polling
│   │   └── handler.go          # HTTP handlers + HTML pages (station, call log)
│   └── fleetmock/
│       └── server.go           # Fleet mock API + control page
├── internal/callservice/tests/
│   └── integration_test.go     # P5: TestDoublePress + TestReplyLostAfterDispatch
├── design/
│   └── DESIGN.md               # Design document (D1–D8)
├── docs/
│   ├── SYSTEM_DESIGN_AND_DISCUSSION_GUIDE.md # 45-min interview defense & architecture guide
│   └── initial_artifact.md     # Initial problem breakdown & planning artifact
├── migrations/
│   └── 001_init.sql            # PostgreSQL schema reference
├── stations.yaml               # Station configuration
├── docker-compose.yml
├── Dockerfile.callservice
├── Dockerfile.fleetmock
└── README.md
```

---

## Environment Variables

### Call Service
| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP port |
| `DATABASE_URL` | `postgres://callstation:callstation@localhost:5432/callstation?sslmode=disable` | PostgreSQL DSN |
| `FLEET_URL` | `http://localhost:8081` | Fleet mock base URL |
| `STATIONS_FILE` | `stations.yaml` | Path to station config |

### Fleet Mock
| Variable | Default | Description |
|----------|---------|-------------|
| `FLEET_PORT` | `8081` | HTTP port |

---

## Design Documents & Interview Defense Guide

- **Design Document (D1–D8):** [`design/DESIGN.md`](design/DESIGN.md)
- **Comprehensive Architecture & 45-Min Interview Defense Guide:** [`docs/SYSTEM_DESIGN_AND_DISCUSSION_GUIDE.md`](docs/SYSTEM_DESIGN_AND_DISCUSSION_GUIDE.md)
- **Initial Assignment Breakdown & Planning:** [`docs/initial_artifact.md`](docs/initial_artifact.md)

---

## Differences Between Prototype and Design

| Design Specification | Prototype Implementation | Reason |
|---------------------|--------------------------|--------|
| **mTLS** (client certificates per station) | Pre-shared Bearer token per station | mTLS requires a CA, cert issuance infrastructure, and TLS termination — out of scope for a 6-hour prototype |
| **Horizontal scaling** with sticky sessions | Single-process instance | Render.com free tier; single-process sufficient for prototype |
| **Prometheus metrics** | Structured logging only | No monitoring infra needed for prototype review |
| **Redis Pub/Sub** for multi-instance SSE | In-process SSE broker (channel per subscriber) | Single instance makes distributed broker unnecessary |
| **OTA firmware updates** | N/A (no hardware) | Out of scope per assignment |
| **Priority queue** for station scheduling | FIFO by `created_at` | Simple queue sufficient for ≤10 stations in prototype |

---

## Disclosure

- **AI assistance:** GitHub Copilot and Antigravity AI were used for code generation and review. All code has been reviewed, understood, and can be fully explained and defended.
- **Libraries used:**
  - `github.com/lib/pq` — PostgreSQL driver (standard, well-established)
  - `github.com/google/uuid` — UUID v4 generation for `requestId`
  - `gopkg.in/yaml.v3` — YAML config parsing
  - Standard library only for HTTP, templates, testing
- **No external templates** were used for the HTML pages.
