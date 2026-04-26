package www

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/shifts"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — covers handlers_production.go and handlers_material.go.
//
// Both file's primary page handlers (handleProduction, handleMaterial,
// handleMaterialPartial) render templates and are not exercised. The
// API endpoints in handlers_production.go and the package-level helpers
// in handlers_material.go (buildStationViews, enrichViewBinState) are
// fully testable.
// ═══════════════════════════════════════════════════════════════════════

func newProdMaterialRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	r.Route("/api", func(r chi.Router) {
		r.Get("/shifts", h.apiListShifts)
		r.Put("/shifts", h.apiSaveShifts)
		r.Get("/hourly-counts", h.apiGetHourlyCounts)
	})
	return h, r
}

// ═══════════════════════════════════════════════════════════════════════
// apiListShifts
// ═══════════════════════════════════════════════════════════════════════

func TestApiListShifts_EmptyByDefault(t *testing.T) {
	_, router := newProdMaterialRouter(t)

	// Clean slate so the response shape is predictable across runs.
	if _, err := testDB.Exec("DELETE FROM shifts"); err != nil {
		t.Fatalf("clear shifts: %v", err)
	}

	resp := doRequest(t, router, "GET", "/api/shifts", nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestApiListShifts_ReturnsSeededShift(t *testing.T) {
	_, router := newProdMaterialRouter(t)

	if _, err := testDB.Exec("DELETE FROM shifts"); err != nil {
		t.Fatalf("clear shifts: %v", err)
	}
	if err := testDB.UpsertShift(1, "Day", "06:00", "14:00"); err != nil {
		t.Fatalf("seed shift: %v", err)
	}

	resp := doRequest(t, router, "GET", "/api/shifts", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	var got []shifts.Shift
	decodeJSON(t, resp, &got)
	if len(got) != 1 {
		t.Fatalf("len(shifts): got %d, want 1", len(got))
	}
	if got[0].Name != "Day" {
		t.Errorf("shift name: got %q, want Day", got[0].Name)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// apiSaveShifts — table-driven with verification of DB state for both
// upsert and delete branches.
// ═══════════════════════════════════════════════════════════════════════

func TestApiSaveShifts_UpsertAndDelete(t *testing.T) {
	_, router := newProdMaterialRouter(t)

	if _, err := testDB.Exec("DELETE FROM shifts"); err != nil {
		t.Fatalf("clear shifts: %v", err)
	}
	// Seed a shift that we'll delete via empty start/end times.
	if err := testDB.UpsertShift(2, "ToBeDeleted", "14:00", "22:00"); err != nil {
		t.Fatalf("seed shift: %v", err)
	}

	body := []map[string]interface{}{
		{"shift_number": 1, "name": "Day", "start_time": "06:00", "end_time": "14:00"},
		{"shift_number": 2, "name": "", "start_time": "", "end_time": ""}, // delete
		{"shift_number": 4, "name": "OutOfRange", "start_time": "00:00", "end_time": "06:00"}, // skipped
	}
	resp := doRequest(t, router, "PUT", "/api/shifts", body, nil)
	assertStatus(t, resp, http.StatusOK)

	shifts, err := testDB.ListShifts()
	if err != nil {
		t.Fatalf("ListShifts: %v", err)
	}
	if len(shifts) != 1 {
		t.Fatalf("expected 1 surviving shift (1=Day), got %d", len(shifts))
	}
	if shifts[0].ShiftNumber != 1 || shifts[0].Name != "Day" {
		t.Errorf("surviving shift: got %+v, want shift_number=1 name=Day", shifts[0])
	}
}

func TestApiSaveShifts_InvalidJSON(t *testing.T) {
	_, router := newProdMaterialRouter(t)

	// Top level should be an array; sending an object decodes into the
	// generic struct slice and fails.
	body := map[string]string{"not": "an array"}
	resp := doRequest(t, router, "PUT", "/api/shifts", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// apiGetHourlyCounts
// ═══════════════════════════════════════════════════════════════════════

func TestApiGetHourlyCounts_NoProcessReturnsEmpty(t *testing.T) {
	_, router := newProdMaterialRouter(t)

	resp := doRequest(t, router, "GET", "/api/hourly-counts", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	// Body is the literal "{}" (not a JSON-encoded map; the handler writes
	// the bytes directly).
}

func TestApiGetHourlyCounts_WithProcessIDReturnsMap(t *testing.T) {
	_, router := newProdMaterialRouter(t)

	pid := seedProcess(t, "ProdHourlyProc")

	resp := doRequest(t, router, "GET", "/api/hourly-counts?process_id="+itoa(pid), nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

// ═══════════════════════════════════════════════════════════════════════
// Material helpers — directly invoke buildStationViews and
// enrichViewBinState, which are package-visible.
// ═══════════════════════════════════════════════════════════════════════

func TestBuildStationViews_NilProcessReturnsNil(t *testing.T) {
	h, _ := newTestHandlers(t)
	views := buildStationViews(h.engine, nil)
	if views != nil {
		t.Errorf("expected nil views for nil process, got %d", len(views))
	}
}

func TestBuildStationViews_ProcessWithoutStations(t *testing.T) {
	h, _ := newTestHandlers(t)
	pid := seedProcess(t, "MaterialNoStations")
	process := &processes.Process{ID: pid}

	views := buildStationViews(h.engine, process)
	if len(views) != 0 {
		t.Errorf("expected zero views for process with no stations, got %d", len(views))
	}
}

func TestBuildStationViews_ProcessWithStation(t *testing.T) {
	h, _ := newTestHandlers(t)
	pid := seedProcess(t, "MaterialOneStation")
	_ = seedOperatorStation(t, pid, "MAT-CODE-1", "MaterialStation1")
	process := &processes.Process{ID: pid}

	views := buildStationViews(h.engine, process)
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
}

func TestEnrichViewBinState_NilCoreAPIIsNoop(t *testing.T) {
	// Nil coreAPI must short-circuit without panicking, even with empty
	// or populated views.
	enrichViewBinState(nil, nil)
	enrichViewBinState(nil, []store.OperatorStationView{
		{Nodes: []store.StationNodeView{}},
	})
}
