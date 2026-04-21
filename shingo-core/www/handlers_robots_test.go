//go:build docker

package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// Characterization tests for handlers_robots.go — renders /robots, returns
// cached robot statuses as JSON, and toggles availability / retries / force
// completes via fleet.RobotLister. The simulator backend does not implement
// RobotLister, so a fake wrapper (fakeRobotListerFleet) is defined below for
// the happy-path tests.

// --- fakeRobotListerFleet ---------------------------------------------------

type fakeRobotListerFleet struct {
	*simulator.SimulatorBackend

	mu              sync.Mutex
	robots          []fleet.RobotStatus
	setAvailErr     error
	setAvailCalls   []robotCall
	retryErr        error
	retryCalls      []string
	forceCompleteErr error
	forceCompleteCalls []string
}

type robotCall struct {
	vehicleID string
	available bool
}

var _ fleet.Backend = (*fakeRobotListerFleet)(nil)
var _ fleet.RobotLister = (*fakeRobotListerFleet)(nil)

func newFakeRobotListerFleet() *fakeRobotListerFleet {
	return &fakeRobotListerFleet{SimulatorBackend: simulator.New()}
}

func (f *fakeRobotListerFleet) GetRobotsStatus() ([]fleet.RobotStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.RobotStatus, len(f.robots))
	copy(out, f.robots)
	return out, nil
}

func (f *fakeRobotListerFleet) SetAvailability(vehicleID string, available bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setAvailCalls = append(f.setAvailCalls, robotCall{vehicleID, available})
	return f.setAvailErr
}

func (f *fakeRobotListerFleet) RetryFailed(vehicleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryCalls = append(f.retryCalls, vehicleID)
	return f.retryErr
}

func (f *fakeRobotListerFleet) ForceComplete(vehicleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceCompleteCalls = append(f.forceCompleteCalls, vehicleID)
	return f.forceCompleteErr
}

// testHandlersWithRobotFleet builds a Handlers whose engine Fleet() is a
// fakeRobotListerFleet. This lets the protected write endpoints reach their
// happy path.
func testHandlersWithRobotFleet(t *testing.T) (*Handlers, *store.DB, *fakeRobotListerFleet) {
	t.Helper()

	db := testdb.Open(t)
	sim := newFakeRobotListerFleet()

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"

	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: nil,
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })

	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })

	dbgLog, _ := debuglog.New(64, nil)

	h := &Handlers{
		engine:   eng,
		sessions: newSessionStore("test-secret"),
		tmpls:    make(map[string]*template.Template),
		eventHub: hub,
		debugLog: dbgLog,
	}
	loadTestTemplates(t, h)
	return h, db, sim
}

// --- handleRobots -----------------------------------------------------------

// TestHandleRobots_RendersHTML pins that /robots renders the template, using
// the engine's cached robot list (empty in a fresh engine).
func TestHandleRobots_RendersHTML(t *testing.T) {
	h, _ := testHandlersForPages(t)

	req := httptest.NewRequest(http.MethodGet, "/robots", nil)
	rec := httptest.NewRecorder()
	h.handleRobots(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Robots") {
		t.Errorf("expected 'Robots' heading in body; len=%d", len(body))
	}
	// A freshly started engine's robot cache is empty → the header count span
	// says "0 robots".
	if !strings.Contains(body, "0 robots") {
		t.Errorf("expected '0 robots' indicator in rendered body; not found")
	}
}

// --- apiRobotsStatus --------------------------------------------------------

// TestApiRobotsStatus_EmptyCache pins the JSON shape on an engine with no
// cached robots: a (possibly null/empty) JSON list.
func TestApiRobotsStatus_EmptyCache(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiRobotsStatus, "/api/robots")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var robots []fleet.RobotStatus
	if err := json.NewDecoder(rec.Body).Decode(&robots); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(robots) != 0 {
		t.Errorf("expected empty robot list, got %d", len(robots))
	}
}

// --- apiRobotSetAvailability -----------------------------------------------

// TestApiRobotSetAvailability_BackendNotSupported pins the 501 path when the
// fleet backend doesn't satisfy RobotLister.
func TestApiRobotSetAvailability_BackendNotSupported(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiRobotSetAvailability, "/api/robots/availability",
		map[string]any{"vehicle_id": "R1", "available": true})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "does not support robot management")
}

// TestApiRobotSetAvailability_InvalidJSON pins 400 on bad body.
func TestApiRobotSetAvailability_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postRaw(t, h.apiRobotSetAvailability, "/api/robots/availability", []byte("not-json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiRobotSetAvailability_HappyPath pins the forward: the handler calls
// RobotLister.SetAvailability with the body's (vehicle_id, available) pair
// and returns {"status":"ok"}.
func TestApiRobotSetAvailability_HappyPath(t *testing.T) {
	h, _, sim := testHandlersWithRobotFleet(t)

	rec := postJSON(t, h.apiRobotSetAvailability, "/api/robots/availability",
		map[string]any{"vehicle_id": "R1", "available": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	sim.mu.Lock()
	defer sim.mu.Unlock()
	if len(sim.setAvailCalls) != 1 ||
		sim.setAvailCalls[0].vehicleID != "R1" ||
		sim.setAvailCalls[0].available != false {
		t.Errorf("expected one SetAvailability(R1,false) call; got %+v", sim.setAvailCalls)
	}
}

// TestApiRobotSetAvailability_BackendError pins the 500 propagation.
func TestApiRobotSetAvailability_BackendError(t *testing.T) {
	h, _, sim := testHandlersWithRobotFleet(t)
	sim.setAvailErr = errStub("robot not found")

	rec := postJSON(t, h.apiRobotSetAvailability, "/api/robots/availability",
		map[string]any{"vehicle_id": "ghost", "available": true})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "robot not found")
}

// --- apiRobotRetryFailed ---------------------------------------------------

func TestApiRobotRetryFailed_BackendNotSupported(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiRobotRetryFailed, "/api/robots/retry",
		map[string]any{"vehicle_id": "R1"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiRobotRetryFailed_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postRaw(t, h.apiRobotRetryFailed, "/api/robots/retry", []byte("bad"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiRobotRetryFailed_HappyPath(t *testing.T) {
	h, _, sim := testHandlersWithRobotFleet(t)

	rec := postJSON(t, h.apiRobotRetryFailed, "/api/robots/retry",
		map[string]any{"vehicle_id": "R2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	sim.mu.Lock()
	defer sim.mu.Unlock()
	if len(sim.retryCalls) != 1 || sim.retryCalls[0] != "R2" {
		t.Errorf("expected RetryFailed(R2); got %v", sim.retryCalls)
	}
}

func TestApiRobotRetryFailed_BackendError(t *testing.T) {
	h, _, sim := testHandlersWithRobotFleet(t)
	sim.retryErr = errStub("nothing to retry")

	rec := postJSON(t, h.apiRobotRetryFailed, "/api/robots/retry",
		map[string]any{"vehicle_id": "R2"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "nothing to retry")
}

// --- apiRobotForceComplete -------------------------------------------------

func TestApiRobotForceComplete_BackendNotSupported(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiRobotForceComplete, "/api/robots/force-complete",
		map[string]any{"vehicle_id": "R1"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiRobotForceComplete_InvalidJSON(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postRaw(t, h.apiRobotForceComplete, "/api/robots/force-complete", []byte("{"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestApiRobotForceComplete_HappyPath(t *testing.T) {
	h, _, sim := testHandlersWithRobotFleet(t)

	rec := postJSON(t, h.apiRobotForceComplete, "/api/robots/force-complete",
		map[string]any{"vehicle_id": "R3"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	sim.mu.Lock()
	defer sim.mu.Unlock()
	if len(sim.forceCompleteCalls) != 1 || sim.forceCompleteCalls[0] != "R3" {
		t.Errorf("expected ForceComplete(R3); got %v", sim.forceCompleteCalls)
	}
}

func TestApiRobotForceComplete_BackendError(t *testing.T) {
	h, _, sim := testHandlersWithRobotFleet(t)
	sim.forceCompleteErr = errStub("stuck")

	rec := postJSON(t, h.apiRobotForceComplete, "/api/robots/force-complete",
		map[string]any{"vehicle_id": "R3"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "stuck")
}
