//go:build docker

package www

import (
	"encoding/json"
	"html/template"
	"net/http"
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

// Characterization tests for handlers_firealarm.go — the fire alarm status +
// trigger endpoints. Both endpoints are gated by the FireAlarm.Enabled config
// feature flag AND require a fleet backend that implements
// fleet.FireAlarmController. The simulator backend does NOT satisfy the
// controller interface, so a thin wrapper (fakeFireAlarmFleet) is defined
// below for the happy-path tests.

// --- fakeFireAlarmFleet ------------------------------------------------------

// fakeFireAlarmFleet wraps the simulator backend and adds FireAlarmController
// semantics. It records the last SetFireAlarm call so tests can assert on the
// handler's behavior end-to-end (without standing up a real RDS).
type fakeFireAlarmFleet struct {
	*simulator.SimulatorBackend

	mu         sync.Mutex
	status     fleet.FireAlarmStatus
	setErr     error // if non-nil, SetFireAlarm returns this error
	getErr     error // if non-nil, GetFireAlarmStatus returns this error
	setOn      bool
	setAuto    bool
	setCallCnt int
}

func newFakeFireAlarmFleet() *fakeFireAlarmFleet {
	return &fakeFireAlarmFleet{
		SimulatorBackend: simulator.New(),
	}
}

// Compile-time check that the fake satisfies both interfaces.
var _ fleet.Backend = (*fakeFireAlarmFleet)(nil)
var _ fleet.FireAlarmController = (*fakeFireAlarmFleet)(nil)

func (f *fakeFireAlarmFleet) GetFireAlarmStatus() (*fleet.FireAlarmStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	s := f.status
	return &s, nil
}

func (f *fakeFireAlarmFleet) SetFireAlarm(on bool, autoResume bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCallCnt++
	f.setOn = on
	f.setAuto = autoResume
	if f.setErr != nil {
		return f.setErr
	}
	f.status = fleet.FireAlarmStatus{
		IsFire:    on,
		ChangedAt: "2025-01-01T00:00:00Z",
	}
	return nil
}

// testHandlersWithFireAlarmFleet builds handlers whose engine Fleet() is a
// fakeFireAlarmFleet. The FireAlarm feature is also enabled on the config so
// the handler passes its feature gate.
func testHandlersWithFireAlarmFleet(t *testing.T, enabled bool) (*Handlers, *store.DB, *fakeFireAlarmFleet) {
	t.Helper()

	db := testdb.Open(t)
	sim := newFakeFireAlarmFleet()

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"
	cfg.FireAlarm.Enabled = enabled

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
	return h, db, sim
}

// --- apiFireAlarmStatus ------------------------------------------------------

// TestApiFireAlarmStatus_DisabledReturns404 pins the feature-gate: if
// FireAlarm.Enabled is false, the endpoint returns 404 regardless of whether
// the fleet supports the controller interface.
func TestApiFireAlarmStatus_DisabledReturns404(t *testing.T) {
	h, _ := testHandlers(t) // simulator backend; FireAlarm.Enabled defaults to false

	rec := getPlain(t, h.apiFireAlarmStatus, "/api/fire-alarm/status")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "disabled")
}

// TestApiFireAlarmStatus_BackendNotSupported pins the interface-assert: when
// the feature is enabled but the backend doesn't implement FireAlarmController,
// the handler returns 501.
func TestApiFireAlarmStatus_BackendNotSupported(t *testing.T) {
	h, db := testHandlers(t)
	_ = db
	// Flip the feature on via the live AppConfig.
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.FireAlarm.Enabled = true
	cfg.Unlock()

	rec := getPlain(t, h.apiFireAlarmStatus, "/api/fire-alarm/status")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "does not support fire alarm")
}

// TestApiFireAlarmStatus_HappyPath pins the success shape: the handler
// forwards GetFireAlarmStatus output into the response body with is_fire and
// changed_at keys.
func TestApiFireAlarmStatus_HappyPath(t *testing.T) {
	h, _, sim := testHandlersWithFireAlarmFleet(t, true)
	// Seed the simulator's fire alarm state.
	sim.status = fleet.FireAlarmStatus{IsFire: true, ChangedAt: "2025-01-01T10:00:00Z"}

	rec := getPlain(t, h.apiFireAlarmStatus, "/api/fire-alarm/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["is_fire"] != true {
		t.Errorf("is_fire: got %v, want true", resp["is_fire"])
	}
	if resp["changed_at"] != "2025-01-01T10:00:00Z" {
		t.Errorf("changed_at: got %v", resp["changed_at"])
	}
}

// TestApiFireAlarmStatus_BackendError pins the bad-gateway path: a controller
// error is forwarded as 502.
func TestApiFireAlarmStatus_BackendError(t *testing.T) {
	h, _, sim := testHandlersWithFireAlarmFleet(t, true)
	sim.getErr = errStub("vendor unavailable")

	rec := getPlain(t, h.apiFireAlarmStatus, "/api/fire-alarm/status")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "vendor unavailable")
}

// --- apiFireAlarmTrigger -----------------------------------------------------

// TestApiFireAlarmTrigger_DisabledReturns404 pins the feature-gate on the
// write path.
func TestApiFireAlarmTrigger_DisabledReturns404(t *testing.T) {
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiFireAlarmTrigger, "/api/fire-alarm/trigger",
		map[string]any{"on": true, "autoResume": false})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiFireAlarmTrigger_InvalidJSON pins the 400 on bad body.
func TestApiFireAlarmTrigger_InvalidJSON(t *testing.T) {
	h, _, _ := testHandlersWithFireAlarmFleet(t, true)
	rec := postRaw(t, h.apiFireAlarmTrigger, "/api/fire-alarm/trigger", []byte("not-json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiFireAlarmTrigger_BackendNotSupported pins the 501 path when the
// backend doesn't satisfy FireAlarmController.
func TestApiFireAlarmTrigger_BackendNotSupported(t *testing.T) {
	h, _ := testHandlers(t)
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.FireAlarm.Enabled = true
	cfg.Unlock()

	rec := postJSON(t, h.apiFireAlarmTrigger, "/api/fire-alarm/trigger",
		map[string]any{"on": true, "autoResume": false})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiFireAlarmTrigger_HappyPath pins the write contract: the handler
// forwards the (on, autoResume) pair to the controller, writes an audit row,
// and returns {"status":"ok"}.
func TestApiFireAlarmTrigger_HappyPath(t *testing.T) {
	h, db, sim := testHandlersWithFireAlarmFleet(t, true)

	rec := postJSON(t, h.apiFireAlarmTrigger, "/api/fire-alarm/trigger",
		map[string]any{"on": true, "autoResume": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONStatus(t, rec.Body.Bytes(), "ok")

	sim.mu.Lock()
	defer sim.mu.Unlock()
	if sim.setCallCnt != 1 || !sim.setOn || !sim.setAuto {
		t.Errorf("controller state after trigger: callCnt=%d on=%v auto=%v",
			sim.setCallCnt, sim.setOn, sim.setAuto)
	}

	// An audit row should have been appended for the firealarm entity.
	entries, err := db.ListEntityAudit("firealarm", 0)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	foundActivated := false
	for _, e := range entries {
		if e.Action == "activated" {
			foundActivated = true
		}
	}
	if !foundActivated {
		t.Errorf("expected 'activated' audit entry, got: %v", entries)
	}
}

// TestApiFireAlarmTrigger_ClearsTheAlarm pins the off path: on=false goes to
// SetFireAlarm with autoResume=false and writes a "cleared" audit row.
func TestApiFireAlarmTrigger_ClearsTheAlarm(t *testing.T) {
	h, db, sim := testHandlersWithFireAlarmFleet(t, true)

	rec := postJSON(t, h.apiFireAlarmTrigger, "/api/fire-alarm/trigger",
		map[string]any{"on": false, "autoResume": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	sim.mu.Lock()
	defer sim.mu.Unlock()
	if sim.setCallCnt != 1 || sim.setOn {
		t.Errorf("controller: want on=false once; got callCnt=%d on=%v", sim.setCallCnt, sim.setOn)
	}
	entries, _ := db.ListEntityAudit("firealarm", 0)
	foundCleared := false
	for _, e := range entries {
		if e.Action == "cleared" {
			foundCleared = true
		}
	}
	if !foundCleared {
		t.Errorf("expected 'cleared' audit entry, got: %v", entries)
	}
}

// TestApiFireAlarmTrigger_BackendError pins the 502 propagation when the
// controller returns an error.
func TestApiFireAlarmTrigger_BackendError(t *testing.T) {
	h, _, sim := testHandlersWithFireAlarmFleet(t, true)
	sim.setErr = errStub("vendor rejected")

	rec := postJSON(t, h.apiFireAlarmTrigger, "/api/fire-alarm/trigger",
		map[string]any{"on": true, "autoResume": false})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "vendor rejected")
}

// errStub is a tiny error type used by fake backends to inject controlled
// failures. Keeping it here (rather than in a shared helper) avoids pulling
// in errors.New at a package-wide level for a single use.
type errStub string

func (e errStub) Error() string { return string(e) }
