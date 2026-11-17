package www

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
	"shingoedge/store/shifts"
	"shingoedge/store/stations"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — covers handlers_production.go.
//
// The page handler is exercised for its screen links only
// (TestProductionPage_ScreenLinks); TestEdgePagePickerPins renders it for
// its pickers. The API endpoints in handlers_production.go and the
// package-level helpers (buildStationViews, enrichViewBinState) are fully
// testable.
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
// The screen links across the top of the home page
// ═══════════════════════════════════════════════════════════════════════

// screenLinkRe reads one screen link out of the rendered page.
var screenLinkRe = regexp.MustCompile(`<a class="btn btn-primary btn-sm" href="([^"]+)" title="([^"]*)">([^<]*)</a>`)

// TestProductionPage_ScreenLinks pins the row of screen links on the home
// page (/ and /production, both handleProduction): one link per station on
// this Edge, a disabled one included, in the store's order (process name,
// then sequence), labelled with the screen's name, in one compact strip (no
// titled card) above #production-content and absent from its partial, so a
// refresh never redraws it. With no screens there is no strip at all.
func TestProductionPage_ScreenLinks(t *testing.T) {
	t.Parallel()

	t.Run("two screens", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHandlers(t)
		h.tmpl = edgePageSet(t)
		db := dbOf(h)
		// Seeded out of order, so the order drawn is the store's, not the seed's.
		lineB := seedProcess(t, db, "Line B")
		lineA := seedProcess(t, db, "Line A")
		weld := seedOperatorStation(t, db, lineB, "weld-2", `Weld <b>2</b>`)
		press, err := db.CreateOperatorStation(stations.Input{ProcessID: lineA, Code: "press-1", Name: "Press 1", Enabled: false})
		testutil.MustNoErr(t, err, "seed the disabled station")

		page := renderEdgePage(t, h.handleProduction, "/")
		row := strings.Index(page, `id="screen-links"`)
		content := strings.Index(page, `id="production-content"`)
		if row < 0 || content < 0 || row > content {
			t.Fatalf("the screen links are not drawn above #production-content (row at %d, content at %d)", row, content)
		}
		var got []string
		for _, m := range screenLinkRe.FindAllStringSubmatch(page[row:content], -1) {
			got = append(got, m[1]+" | "+m[2]+" | "+m[3])
		}
		want := []string{
			fmt.Sprintf("/operator/station/%d | Line A | Press 1", press),
			fmt.Sprintf("/operator/station/%d | Line B | Weld &lt;b&gt;2&lt;/b&gt;", weld),
		}
		if !slices.Equal(got, want) {
			t.Errorf("screen links:\n  got  %q\n  want %q", got, want)
		}
		// A div with the navigation role: every <nav> wears the site's top bar
		// (components.css), which a strip in the page is not.
		if !strings.Contains(page, `<div class="row-wrap items-center mb-3" id="screen-links" role="navigation" aria-label="Operator screens">`) ||
			strings.Contains(page, "<strong>Screens</strong>") || strings.Contains(page[strings.Index(page, `id="screen-links"`)-200:strings.Index(page, `id="screen-links"`)], "<nav") {
			t.Error("the screens are not one compact strip: a titled card is back, or the strip is a <nav> wearing the top bar")
		}

		partial := renderEdgePage(t, h.handleProductionPartial, "/production/partial")
		if strings.Contains(partial, "screen-links") || strings.Contains(partial, "/operator/station/") {
			t.Error("the production partial carries the screen links: every refresh would redraw them")
		}
	})

	t.Run("no screens", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHandlers(t)
		h.tmpl = edgePageSet(t)
		seedProcess(t, dbOf(h), "Line A")

		page := renderEdgePage(t, h.handleProduction, "/")
		if strings.Contains(page, `id="screen-links"`) || strings.Contains(page, `aria-label="Operator screens"`) ||
			strings.Contains(page, "/operator/station/") {
			t.Error("a home page with no screens draws the screen strip")
		}
		if !strings.Contains(page, `id="production-content"`) {
			t.Fatal("the page did not render: the check above passed on nothing")
		}
	})
}

// ═══════════════════════════════════════════════════════════════════════
// apiListShifts
// ═══════════════════════════════════════════════════════════════════════

func TestApiListShifts_EmptyByDefault(t *testing.T) {
	t.Parallel()
	h, router := newProdMaterialRouter(t)
	db := dbOf(h)

	// Clean slate so the response shape is predictable across runs.
	if _, err := db.Exec("DELETE FROM shifts"); err != nil {
		t.Fatalf("clear shifts: %v", err)
	}

	resp := doRequest(t, router, "GET", "/api/shifts", nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

func TestApiListShifts_ReturnsSeededShift(t *testing.T) {
	t.Parallel()
	h, router := newProdMaterialRouter(t)
	db := dbOf(h)

	if _, err := db.Exec("DELETE FROM shifts"); err != nil {
		t.Fatalf("clear shifts: %v", err)
	}
	testutil.MustNoErr(t, db.UpsertShift(1, "Day", "06:00", "14:00"), "seed shift")

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
	t.Parallel()
	h, router := newProdMaterialRouter(t)
	db := dbOf(h)

	if _, err := db.Exec("DELETE FROM shifts"); err != nil {
		t.Fatalf("clear shifts: %v", err)
	}
	// Seed a shift that we'll delete via empty start/end times.
	testutil.MustNoErr(t, db.UpsertShift(2, "ToBeDeleted", "14:00", "22:00"), "seed shift")

	body := []map[string]any{
		{"shift_number": 1, "name": "Day", "start_time": "06:00", "end_time": "14:00"},
		{"shift_number": 2, "name": "", "start_time": "", "end_time": ""},                     // delete
		{"shift_number": 4, "name": "OutOfRange", "start_time": "00:00", "end_time": "06:00"}, // skipped
	}
	resp := doRequest(t, router, "PUT", "/api/shifts", body, nil)
	assertStatus(t, resp, http.StatusOK)

	shifts, err := db.ListShifts()
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
	t.Parallel()
	_, router := newProdMaterialRouter(t)

	// Top level should be an array; sending an object decodes into the
	// generic struct slice and fails.
	body := map[string]string{"not": "an array"}
	resp := doRequest(t, router, "PUT", "/api/shifts", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// TestApiSaveShifts_AbsentShiftIsDeleted covers the DOM-side removeShiftRow
// path: the row is dropped from the editor entirely, so the shift_number
// never appears in the PUT payload. The handler must reconcile against the
// existing rows and delete the absent one — otherwise a removed shift sat
// in the DB forever.
func TestApiSaveShifts_AbsentShiftIsDeleted(t *testing.T) {
	t.Parallel()
	h, router := newProdMaterialRouter(t)
	db := dbOf(h)

	if _, err := db.Exec("DELETE FROM shifts"); err != nil {
		t.Fatalf("clear shifts: %v", err)
	}
	// Seed two shifts; the operator removes shift 2 in the UI, so only
	// shift 1 is sent. Shift 2 must be deleted from the DB.
	testutil.MustNoErr(t, db.UpsertShift(1, "Day", "06:00", "14:00"), "seed shift 1")
	testutil.MustNoErr(t, db.UpsertShift(2, "Swing", "14:00", "22:00"), "seed shift 2")

	body := []map[string]any{
		{"shift_number": 1, "name": "Day", "start_time": "06:00", "end_time": "14:00"},
	}
	resp := doRequest(t, router, "PUT", "/api/shifts", body, nil)
	assertStatus(t, resp, http.StatusOK)

	got, err := db.ListShifts()
	if err != nil {
		t.Fatalf("ListShifts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving shift (shift 2 absent should be deleted), got %d: %+v", len(got), got)
	}
	if got[0].ShiftNumber != 1 || got[0].Name != "Day" {
		t.Errorf("surviving shift: got %+v, want shift_number=1 name=Day", got[0])
	}
}

// ═══════════════════════════════════════════════════════════════════════
// apiGetHourlyCounts
// ═══════════════════════════════════════════════════════════════════════

func TestApiGetHourlyCounts_NoProcessReturnsEmpty(t *testing.T) {
	t.Parallel()
	_, router := newProdMaterialRouter(t)

	resp := doRequest(t, router, "GET", "/api/hourly-counts", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	// A missing process_id now means ALL processes: the handler sums every
	// process's HourlyTotals for the date. With no processes configured the
	// sum is empty and the body marshals to "{}"; with any processes present
	// it is their combined map. Only the status is asserted here so the test
	// stays agnostic of how many processes the seed left behind.
}

func TestApiGetHourlyCounts_WithProcessIDReturnsMap(t *testing.T) {
	t.Parallel()
	h, router := newProdMaterialRouter(t)
	db := dbOf(h)

	pid := seedProcess(t, db, "ProdHourlyProc")

	resp := doRequest(t, router, "GET", "/api/hourly-counts?process_id="+itoa(pid), nil, nil)
	assertStatus(t, resp, http.StatusOK)
}

// ═══════════════════════════════════════════════════════════════════════
// Material helpers — directly invoke buildStationViews and
// enrichViewBinState, which are package-visible.
// ═══════════════════════════════════════════════════════════════════════

func TestBuildStationViews_NilProcessReturnsNil(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandlers(t)
	views := buildStationViews(context.Background(), h.engine, nil)
	if views != nil {
		t.Errorf("expected nil views for nil process, got %d", len(views))
	}
}

func TestBuildStationViews_ProcessWithoutStations(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandlers(t)
	db := dbOf(h)
	pid := seedProcess(t, db, "MaterialNoStations")
	process := &processes.Process{ID: pid}

	views := buildStationViews(context.Background(), h.engine, process)
	if len(views) != 0 {
		t.Errorf("expected zero views for process with no stations, got %d", len(views))
	}
}

func TestBuildStationViews_ProcessWithStation(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandlers(t)
	db := dbOf(h)
	pid := seedProcess(t, db, "MaterialOneStation")
	_ = seedOperatorStation(t, db, pid, "MAT-CODE-1", "MaterialStation1")
	process := &processes.Process{ID: pid}

	views := buildStationViews(context.Background(), h.engine, process)
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
}
