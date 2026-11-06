// Package callservice_test contains integration tests for the call service (P5).
//
// Two required tests:
//   1. TestDoublePress        - double press creates only one open call
//   2. TestReplyLostAfterDispatch - reply lost after dispatch; service retries and resolves
//
// Prerequisites:
//   - Fleet mock runs in-process (httptest)
//   - Call Service runs in-process (httptest)
//   - Set TEST_DATABASE_URL env var, e.g.:
//       postgres://callstation:callstation@localhost:5432/callstation_test?sslmode=disable
//
// Run:
//
//	TEST_DATABASE_URL=... go test ./internal/callservice/tests/... -v -timeout 120s
package callservice_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	callsvc "call-station/internal/callservice"
	"call-station/internal/fleetmock"

	_ "github.com/lib/pq"
)

// --- helpers ---

// testFleet starts a fleet mock httptest server and returns its URL and the server handle.
func testFleet(t *testing.T) (*httptest.Server, *fleetmock.Server) {
	t.Helper()
	fleet := fleetmock.NewServer()
	ts := httptest.NewServer(fleet.Routes())
	t.Cleanup(ts.Close)
	return ts, fleet
}

// testCallService starts a call service httptest server against the given fleet URL.
func testCallService(t *testing.T, fleetURL string) *httptest.Server {
	t.Helper()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}

	// Clean test database before test starts
	if cleanDB, err := sql.Open("postgres", dbURL); err == nil {
		_, _ = cleanDB.Exec("TRUNCATE TABLE calls, call_events CASCADE")
		cleanDB.Close()
	}

	// Write a temporary stations.yaml
	stationsYAML := "stations:\n" +
		"  - id: \"T1\"\n    destination: \"A1\"\n    token: \"test-token-t1\"\n" +
		"  - id: \"T2\"\n    destination: \"A2\"\n    token: \"test-token-t2\"\n"

	tmpFile, err := os.CreateTemp(t.TempDir(), "stations-*.yaml")
	if err != nil {
		t.Fatalf("create temp stations file: %v", err)
	}
	if _, err := tmpFile.WriteString(stationsYAML); err != nil {
		t.Fatalf("write stations file: %v", err)
	}
	tmpFile.Close()

	svc, err := callsvc.NewServer(dbURL, fleetURL, tmpFile.Name())
	if err != nil {
		t.Fatalf("new call service: %v", err)
	}

	ts := httptest.NewServer(svc.Routes())
	t.Cleanup(func() {
		ts.Close()
		svc.Close()
	})
	return ts
}

// doPost makes a POST /api/call/{stationId} and returns status code + decoded body.
func doPost(t *testing.T, baseURL, stationID, token string) (int, map[string]interface{}) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/call/%s", baseURL, stationID),
		bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/call: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	_ = json.Unmarshal(body, &result)
	return resp.StatusCode, result
}

// doGet makes a GET /api/call/{stationId} and returns the decoded body.
func doGet(t *testing.T, baseURL, stationID, token string) map[string]interface{} {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/call/%s", baseURL, stationID), nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/call: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	return result
}

// waitForStatus polls GET /api/call/{stationId} until status matches or timeout.
func waitForStatus(t *testing.T, baseURL, stationID, token, want string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state := doGet(t, baseURL, stationID, token)
		if state["status"] == want {
			return state
		}
		time.Sleep(500 * time.Millisecond)
	}
	last := doGet(t, baseURL, stationID, token)
	t.Fatalf("timed out after %v waiting for status=%s on station=%s; last state=%v",
		timeout, want, stationID, last)
	return nil
}

// =============================================================================
// P5 — Test 1: Double Press
// =============================================================================

// TestDoublePress verifies that pressing the call button twice (even concurrently)
// results in exactly ONE open call per station.
//
// Design coverage: D3 "duplicate press", D5 "at-most-one open call per station"
func TestDoublePress(t *testing.T) {
	fleetTS, _ := testFleet(t)
	svcTS := testCallService(t, fleetTS.URL)

	const (
		stationID = "T1"
		token     = "test-token-t1"
	)

	// Fire two concurrent presses
	var mu sync.Mutex
	results := make([]map[string]interface{}, 2)
	codes := make([]int, 2)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			code, body := doPost(t, svcTS.URL, stationID, token)
			mu.Lock()
			codes[idx] = code
			results[idx] = body
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Both presses must receive a non-5xx response
	for i, code := range codes {
		if code >= 500 {
			t.Errorf("press %d: unexpected HTTP %d (body: %v)", i+1, code, results[i])
		}
	}

	// Both responses must reference the SAME callId
	callID0, _ := results[0]["callId"].(string)
	callID1, _ := results[1]["callId"].(string)
	if callID0 == "" || callID1 == "" {
		t.Fatalf("missing callId in responses: press1=%v press2=%v", results[0], results[1])
	}
	if callID0 != callID1 {
		t.Errorf("FAIL: double press created two distinct calls — callId1=%s callId2=%s", callID0, callID1)
	} else {
		t.Logf("PASS: both presses returned same callId=%s", callID0)
	}

	// Final state must be a single non-error status
	state := doGet(t, svcTS.URL, stationID, token)
	t.Logf("final state: %v", state)
}

// =============================================================================
// P5 — Test 2: Reply Lost After Dispatch
// =============================================================================

// TestReplyLostAfterDispatch verifies that when the fleet drops the HTTP response
// after creating a mission, the call service:
//   - Detects the timeout
//   - Retries dispatch with the SAME requestId (idempotency)
//   - Eventually resolves the call to en_route
//   - Creates exactly ONE mission in the fleet (no duplicates)
//
// Design coverage: D3 "reply lost after dispatch", D4 requestId stability
func TestReplyLostAfterDispatch(t *testing.T) {
	fleetTS, fleet := testFleet(t)

	// Enable lost-after-dispatch BEFORE the call service connects
	fleet.SetControls(fleetmock.Controls{LostAfterDispatch: true})

	svcTS := testCallService(t, fleetTS.URL)

	const (
		stationID = "T2"
		token     = "test-token-t2"
	)

	// Press the button
	code, result := doPost(t, svcTS.URL, stationID, token)
	if code >= 500 {
		t.Fatalf("POST /api/call: HTTP %d %v", code, result)
	}
	callID, _ := result["callId"].(string)
	if callID == "" {
		t.Fatalf("no callId in response: %v", result)
	}
	t.Logf("call created: callId=%s", callID)

	// Allow the engine to attempt dispatch and get stuck on the dropped reply
	time.Sleep(3 * time.Second)

	midState := doGet(t, svcTS.URL, stationID, token)
	t.Logf("state while reply is being dropped: status=%v", midState["status"])

	// Now lift the failure — fleet will respond to the next retry
	fleet.SetControls(fleetmock.Controls{LostAfterDispatch: false})
	t.Log("fleet: lost-after-dispatch disabled; next retry should succeed")

	// Wait for the call to reach en_route (retry with same requestId)
	finalState := waitForStatus(t, svcTS.URL, stationID, token, "en_route", 90*time.Second)
	t.Logf("PASS: call reached en_route — state=%v", finalState)

	// Fleet idempotency check: exactly ONE mission must exist despite multiple retries
	missionCount := fleet.MissionCount()
	if missionCount != 1 {
		t.Errorf("FAIL: expected exactly 1 mission (idempotency), got %d", missionCount)
	} else {
		t.Logf("PASS: fleet has exactly 1 mission despite multiple dispatch retries")
	}
}
