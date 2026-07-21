package www

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingoedge/domain"
)

// handlers_gate_status_test.go — the read-only cutover gate-status endpoint
// behind the live "waiting on:" panel.

// newGateStatusRouter wires just the gate-status route against the stub
// engine, and hands back the stub so a test can set its canned response.
func newGateStatusRouter(t *testing.T) (*stubEngine, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)
	r.Get("/api/processes/{id}/changeover/gate-status", h.apiChangeoverGateStatus)
	return h.engine.(*stubEngine), r
}

func gateStatusGet(t *testing.T, r http.Handler, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode gate-status body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, body
}

// TestGateStatus_OpenGate_ReturnsEmptyArrayNotNull pins the JSON shape. The
// panel iterates blockers; `null` would force every consumer to nil-check
// before ranging, so an unblocked gate must encode as [].
func TestGateStatus_OpenGate_ReturnsEmptyArrayNotNull(t *testing.T) {
	eng, r := newGateStatusRouter(t)
	eng.gateCanComplete = true
	eng.gateBlockers = nil

	rec, body := gateStatusGet(t, r, "/api/processes/1/changeover/gate-status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if body["can_complete"] != true {
		t.Errorf("can_complete = %v, want true", body["can_complete"])
	}
	blockers, ok := body["blockers"].([]any)
	if !ok {
		t.Fatalf("blockers is %T, want a JSON array (null would force a nil-check on every consumer)", body["blockers"])
	}
	if len(blockers) != 0 {
		t.Errorf("blockers = %v, want empty", blockers)
	}
}

// TestGateStatus_BlockedGate_SurfacesEveryBlocker checks the panel gets both
// the sentence and the structured identity it links from.
func TestGateStatus_BlockedGate_SurfacesEveryBlocker(t *testing.T) {
	eng, r := newGateStatusRouter(t)
	eng.gateCanComplete = false
	eng.gateBlockers = []domain.Blocker{
		{Reason: "task at node ALN_002 in staging_requested", NodeName: "ALN_002", Hard: true},
		{Reason: "order 703 in in_transit", OrderID: 703, Hard: true},
	}

	rec, body := gateStatusGet(t, r, "/api/processes/1/changeover/gate-status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body["can_complete"] != false {
		t.Errorf("can_complete = %v, want false", body["can_complete"])
	}
	blockers, _ := body["blockers"].([]any)
	if len(blockers) != 2 {
		t.Fatalf("blockers length = %d, want 2", len(blockers))
	}

	first, _ := blockers[0].(map[string]any)
	if first["reason"] != "task at node ALN_002 in staging_requested" {
		t.Errorf("blocker[0].reason = %v", first["reason"])
	}
	if first["node_name"] != "ALN_002" {
		t.Errorf("blocker[0].node_name = %v, want ALN_002", first["node_name"])
	}
	if first["hard"] != true {
		t.Errorf("blocker[0].hard = %v, want true", first["hard"])
	}
	if _, present := first["order_id"]; present {
		t.Errorf("blocker[0] carries order_id on a task blocker: %v", first["order_id"])
	}

	second, _ := blockers[1].(map[string]any)
	if second["order_id"] != float64(703) {
		t.Errorf("blocker[1].order_id = %v, want 703", second["order_id"])
	}
	if _, present := second["node_name"]; present {
		t.Errorf("blocker[1] carries node_name on an order blocker: %v", second["node_name"])
	}
}

// TestGateStatus_EngineError_Is500 — a real read failure must not be dressed
// up as an open gate, which would tell the operator "nothing pending" while
// the truth is unknown.
func TestGateStatus_EngineError_Is500(t *testing.T) {
	eng, r := newGateStatusRouter(t)
	eng.gateErr = errors.New("db exploded")

	rec, _ := gateStatusGet(t, r, "/api/processes/1/changeover/gate-status")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestGateStatus_InvalidProcessID_Is400 covers the parse guard.
func TestGateStatus_InvalidProcessID_Is400(t *testing.T) {
	_, r := newGateStatusRouter(t)
	rec, _ := gateStatusGet(t, r, "/api/processes/not-a-number/changeover/gate-status")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestGateStatus_IsGetOnly — the endpoint must not be reachable by POST. It is
// advertised as safe to poll; a mutating verb sharing the path would undo that
// guarantee for anything that discovers the route by pattern.
func TestGateStatus_IsGetOnly(t *testing.T) {
	_, r := newGateStatusRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/processes/1/changeover/gate-status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("POST to gate-status returned 200; it is a read-only endpoint")
	}
}
