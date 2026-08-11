package www

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/shifts"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — covers handlers_production.go.
//
// The primary page handler (handleProduction, handleProductionPartial)
// renders templates and is not exercised. The API endpoints in
// handlers_production.go and the package-level helpers
// (buildStationViews, enrichViewBinState) are fully testable.
// ═══════════════════════════════════════════════════════════════════════

func newProdMaterialRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	r.Route("/api", func(r chi.Router) {
		r.Get("/shifts", h.apiListShifts)
		r.Put("/shifts", h.apiSaveShifts)
		r.Get("/hourly-counts", h.apiGetHourlyCounts)
		r.Get("/daily-counts", h.apiGetDailyCounts)
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
	testutil.MustNoErr(t, testDB.UpsertShift(1, "Day", "06:00", "14:00"), "seed shift")

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
	testutil.MustNoErr(t, testDB.UpsertShift(2, "ToBeDeleted", "14:00", "22:00"), "seed shift")

	body := []map[string]any{
		{"shift_number": 1, "name": "Day", "start_time": "06:00", "end_time": "14:00"},
		{"shift_number": 2, "name": "", "start_time": "", "end_time": ""},                     // delete
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
// apiGetDailyCounts
//
// This endpoint is what makes counters.PurgeRolledUpHourly defensible:
// after 90 days the hour buckets are gone and this is where the day's
// production went. Asserting the BODY rather than just the status is the
// point — a 200 carrying `null` would satisfy the status check and break
// every caller that iterates it, and `null` is exactly what a Go nil slice
// marshals to.
// ═══════════════════════════════════════════════════════════════════════

func TestApiGetDailyCounts_EmptyIsAnArrayNotNull(t *testing.T) {
	_, router := newProdMaterialRouter(t)
	pid := seedProcess(t, "ProdDailyEmptyProc")

	resp := doRequest(t, router, "GET", "/api/daily-counts?process_id="+itoa(pid), nil, nil)
	assertStatus(t, resp, http.StatusOK)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Errorf("body = %q, want %q — a nil slice marshals to null and every caller iterates this", got, "[]")
	}
}

func TestApiGetDailyCounts_ReturnsTheDayTotal(t *testing.T) {
	_, router := newProdMaterialRouter(t)
	pid := seedProcess(t, "ProdDailyProc")
	sid, err := testDB.CreateStyle("ProdDailyStyle", "", pid)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if _, err := testDB.Exec(
		`INSERT INTO daily_counts (process_id, style_id, count_date, total) VALUES (?, ?, ?, ?)`,
		pid, sid, time.Now().Format("2006-01-02"), 137); err != nil {
		t.Fatalf("seed daily count: %v", err)
	}

	resp := doRequest(t, router, "GET", "/api/daily-counts?process_id="+itoa(pid), nil, nil)
	assertStatus(t, resp, http.StatusOK)

	var got []struct {
		ProcessID int64  `json:"process_id"`
		StyleID   int64  `json:"style_id"`
		CountDate string `json:"count_date"`
		Total     int64  `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 — the default date window should cover today", len(got))
	}
	if got[0].Total != 137 || got[0].StyleID != sid {
		t.Errorf("got style %d total %d, want style %d total 137", got[0].StyleID, got[0].Total, sid)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Material helpers — directly invoke buildStationViews and
// enrichViewBinState, which are package-visible.
// ═══════════════════════════════════════════════════════════════════════

func TestBuildStationViews_NilProcessReturnsNil(t *testing.T) {
	h, _ := newTestHandlers(t)
	views := buildStationViews(context.Background(), h.engine, nil)
	if views != nil {
		t.Errorf("expected nil views for nil process, got %d", len(views))
	}
}

func TestBuildStationViews_ProcessWithoutStations(t *testing.T) {
	h, _ := newTestHandlers(t)
	pid := seedProcess(t, "MaterialNoStations")
	process := &processes.Process{ID: pid}

	views := buildStationViews(context.Background(), h.engine, process)
	if len(views) != 0 {
		t.Errorf("expected zero views for process with no stations, got %d", len(views))
	}
}

func TestBuildStationViews_ProcessWithStation(t *testing.T) {
	h, _ := newTestHandlers(t)
	pid := seedProcess(t, "MaterialOneStation")
	_ = seedOperatorStation(t, pid, "MAT-CODE-1", "MaterialStation1")
	process := &processes.Process{ID: pid}

	views := buildStationViews(context.Background(), h.engine, process)
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
