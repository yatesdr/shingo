//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/clock"
	"shingocore/service"
	"shingocore/store/heartbeat"
)

var updateCellsGolden = flag.Bool("update", false, "rewrite testdata golden files")

// stripIDs removes every "id" key (BIGSERIAL values) and rewrites every
// timestamp in UTC, so the golden depends on neither sequence state nor the
// zone the database session reports in.
func stripIDs(v any) any {
	switch x := v.(type) {
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return ts.UTC().Format(time.RFC3339Nano)
		}
	case map[string]any:
		delete(x, "id")
		for k, e := range x {
			x[k] = stripIDs(e)
		}
	case []any:
		for i, e := range x {
			x[i] = stripIDs(e)
		}
	}
	return v
}

// TestCellsAPI_Golden (P7) pins the three heartbeat read endpoints over a
// seeded fixture, for an id that is no catalog cell (the station grain):
// /api/cells/{id}/heartbeat and /stops over
// HTTP with an explicit window, and the /state payload through the same
// service call the handler makes, at a fixed "now" (the handler reads the wall
// clock, which a golden cannot). A catalog cell through the same surfaces is
// TestCellsAPI_CatalogCellWithNoConfigRow; the configured cell this pinned went
// with cell_config (v189).
//
// The fixture rows go in by plain SQL naming only the columns every shape of
// cell_part_events has. The stream carries a delta = 3 tick and a jump, so a
// change to how parts are counted shows up here as well as in the pure golden.
func TestCellsAPI_Golden(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)
	t0 := time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC)
	if err := db.EnsureHeartbeatPartitions(t0); err != nil {
		t.Fatalf("partitions: %v", err)
	}
	type ev struct {
		sec     int
		pid     int64
		delta   int64
		anomaly string
	}
	evs := []ev{
		{0, 7, 1, ""}, {20, 7, 1, ""}, {40, 7, 3, ""}, {60, 7, 1, ""}, {80, 7, 700, "jump"},
		{100, 7, 1, ""}, {400, 7, 1, ""}, {420, 7, 1, ""},
		{5, 9, 1, ""}, {50, 9, 2, ""}, {95, 9, 1, ""},
	}
	for i, e := range evs {
		if _, err := db.Exec(`INSERT INTO cell_part_events (station_uid, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id)
			VALUES ('stn-p7', $1, $2, $3, $4, $5, $6, 3)`,
			t0.Add(time.Duration(e.sec)*time.Second), i+1, 100+i, e.delta, e.anomaly, e.pid); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	r := chi.NewRouter()
	r.Get("/api/cells/{id}/heartbeat", h.apiCellHeartbeat)
	r.Get("/api/cells/{id}/stops", h.apiCellStops)
	get := func(path string) any {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		var v any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return stripIDs(v)
	}
	state := func(id string) any {
		s, err := h.engine.HeartbeatService().ResolveCellState(id, t0.Add(430*time.Second))
		if err != nil {
			t.Fatalf("ResolveCellState(%s): %v", id, err)
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal state: %v", err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		return stripIDs(v)
	}
	win := "?since=2026-01-15T07:59:00Z&until=2026-01-15T08:07:10Z"
	got := map[string]any{
		"heartbeat_unconfigured": get("/api/cells/stn-p7/heartbeat" + win),
		"stops_unconfigured":     get("/api/cells/stn-p7/stops" + win),
		"state_unconfigured":     state("stn-p7"),
	}
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	path := filepath.Join("testdata", "cells_api_golden.json")
	if *updateCellsGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), b) {
		t.Errorf("cells API changed. got:\n%s", b)
	}
}

// TestCellState_TargetIsTheStreamMedianWithNoTargetRow pins the target cycle a
// cell's state reads when nothing configures one: heartbeat.EstimateTarget, the
// median per-part gap of the stream. cell_targets never had a writer, so this
// was the only answer its reader ever let through; the table and the reader go
// in v185, and this must read the same across that.
//
// Red when the state's target is anything but the stream's median: a target
// read from somewhere else, a zero, or a different estimate.
func TestCellState_TargetIsTheStreamMedianWithNoTargetRow(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)
	t0 := time.Date(2026, 1, 16, 8, 0, 0, 0, time.UTC)
	if err := db.EnsureHeartbeatPartitions(t0); err != nil {
		t.Fatalf("partitions: %v", err)
	}
	// One part a fire; gaps of 20, 20, 30 and 20 s, so the median is 20 s.
	var evs []heartbeat.PartEvent
	for i, sec := range []int{0, 20, 40, 70, 90} {
		evs = append(evs, heartbeat.PartEvent{StationUID: "stn-target", RecordedAt: t0.Add(time.Duration(sec) * time.Second),
			EdgeSnapshotID: int64(i + 1), CountValue: int64(100 + i), Delta: 1, ProcessID: 7, StyleID: 3})
	}
	if _, rejected := db.InsertCellPartEvents(evs); len(rejected) != 0 {
		t.Fatalf("seed ticks: rejected %+v", rejected)
	}
	want := heartbeat.EstimateTarget(evs)
	if want != 20*time.Second {
		t.Fatalf("fixture: the stream's median is %v, want 20s", want)
	}
	s, err := h.engine.HeartbeatService().ResolveCellState("stn-target", t0.Add(95*time.Second))
	if err != nil {
		t.Fatalf("ResolveCellState: %v", err)
	}
	if got := time.Duration(s.Primary.TargetCycleMS) * time.Millisecond; got != want {
		t.Errorf("target cycle = %v, want the stream's median %v", got, want)
	}
}

// TestCellsAPI_CatalogCellWithNoConfigRow pins what every cell surface answers
// for a cell no one configured: the catalog cell an Edge reported (edge_cells),
// with no cell_config row anywhere. Each read is made as its page makes it, at a
// fixed "now":
//
//   - the heartbeat wall and Missions' cells list: GET /api/cells;
//   - a heartbeat board's cells: GET /api/dashboards/{id}/cells, for a
//     plant-wide board with no setup and for a board scoped to one station
//     that renames one cell and hides another;
//   - a cell tile's state: GET /api/cells/{id}/state;
//   - the wall's rhythm seed and the cell drill: GET /api/cells/{id}/heartbeat,
//     with ?since= (two minutes back) and with no window (the drill's eight
//     hours);
//   - GET /api/cells/{id}/stops (no page reads it today).
//
// The catalog carries the cases a resolve can split on: a cell with a primary
// and a sub process, a single-process cell, one with three processes, a stale
// cell and a cell with no bindings (neither listed; their ids fall back to the
// station grain), and one label on two stations (both listed; the id resolves
// to the first station by name). The fixture rows go in by plain SQL naming
// only the columns every shape of edge_cells and cell_part_events has.
//
// Not parallel: it swaps the default clock, which the parallel tests read.
func TestCellsAPI_CatalogCellWithNoConfigRow(t *testing.T) {
	h, db := testHandlersForPages(t)
	t0 := time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC)
	now := t0.Add(430 * time.Second)
	prevClock := clock.Default()
	clock.SetDefault(clock.NewManual(now))
	t.Cleanup(func() { clock.SetDefault(prevClock) })
	if err := db.EnsureHeartbeatPartitions(t0); err != nil {
		t.Fatalf("partitions: %v", err)
	}

	const stnA, stnB = "stn-cellpin-a", "stn-cellpin-b"
	bind := func(plc string, pids ...int64) string {
		out := []map[string]any{}
		for i, p := range pids {
			out = append(out, map[string]any{"process_id": p, "style_id": 3, "plc_name": plc, "tag_name": "T" + strconv.Itoa(i+1)})
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	catalog := []struct {
		station, label, bindings string
		stale                    bool
	}{
		{stnA, "PLC-ALPHA", bind("PLC-ALPHA", 7, 9), false},
		{stnA, "PLC-BRAVO", bind("PLC-BRAVO", 11), false},
		{stnA, "PLC-RETIRED", bind("PLC-RETIRED", 12), true},
		{stnA, "PLC-UNBOUND", bind("PLC-UNBOUND"), false},
		{stnB, "PLC-ALPHA", bind("PLC-ALPHA", 31), false},
		{stnB, "PLC-CHARLIE", bind("PLC-CHARLIE", 21, 22, 23), false},
	}
	for _, c := range catalog {
		if _, err := db.Exec(`INSERT INTO edge_cells (station_uid, cell_label, bindings, stale) VALUES ($1, $2, $3::jsonb, $4)`,
			c.station, c.label, c.bindings, c.stale); err != nil {
			t.Fatalf("seed catalog %s/%s: %v", c.station, c.label, err)
		}
	}
	type ev struct {
		station string
		sec     int
		pid     int64
		delta   int64
		anomaly string
	}
	evs := []ev{
		{stnA, 0, 7, 1, ""}, {stnA, 20, 7, 1, ""}, {stnA, 40, 7, 3, ""}, {stnA, 60, 7, 1, ""}, {stnA, 80, 7, 700, "jump"},
		{stnA, 400, 7, 1, ""}, {stnA, 420, 7, 1, ""},
		{stnA, 5, 9, 1, ""}, {stnA, 50, 9, 2, ""}, {stnA, 95, 9, 1, ""},
		{stnA, 10, 11, 1, ""}, {stnA, 30, 11, 1, ""},
		{stnA, 35, 12, 1, ""},
		{stnB, 0, 21, 1, ""}, {stnB, 30, 21, 1, ""}, {stnB, 60, 21, 1, ""},
		{stnB, 15, 22, 1, ""},
		{stnB, 25, 31, 1, ""}, {stnB, 45, 31, 1, ""},
	}
	for i, e := range evs {
		if _, err := db.Exec(`INSERT INTO cell_part_events (station_uid, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 3)`,
			e.station, t0.Add(time.Duration(e.sec)*time.Second), i+1, 100+i, e.delta, e.anomaly, e.pid); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	boards := h.engine.DashboardService()
	plantBoard, err := boards.Create(service.DashboardInput{Name: "Cells pin plant", Kind: "heartbeat", Enabled: true})
	if err != nil {
		t.Fatalf("plant board: %v", err)
	}
	stationBoard, err := boards.Create(service.DashboardInput{Name: "Cells pin station", Kind: "heartbeat", Enabled: true,
		Stations: []string{stnA},
		Config:   json.RawMessage(`{"cells":{"PLC-ALPHA":{"name":"Alpha line"},"PLC-BRAVO":{"hide":true}}}`)})
	if err != nil {
		t.Fatalf("station board: %v", err)
	}

	// The input this pins is the absence of a cell_config row: say so, and
	// stay true once the table itself is gone.
	var configTable *string
	if err := db.QueryRow(`SELECT to_regclass('public.cell_config')::text`).Scan(&configTable); err != nil {
		t.Fatalf("look for cell_config: %v", err)
	}
	if configTable != nil {
		var rows int
		if err := db.QueryRow(`SELECT count(*) FROM cell_config`).Scan(&rows); err != nil {
			t.Fatalf("count cell_config: %v", err)
		}
		if rows != 0 {
			t.Fatalf("fixture: cell_config holds %d rows; this pins the answer with none", rows)
		}
	}
	var catalogRows int
	if err := db.QueryRow(`SELECT count(*) FROM edge_cells WHERE station_uid IN ($1, $2)`, stnA, stnB).Scan(&catalogRows); err != nil {
		t.Fatalf("count edge_cells: %v", err)
	}
	if catalogRows != len(catalog) {
		t.Fatalf("fixture: edge_cells holds %d rows for the two stations, want %d", catalogRows, len(catalog))
	}

	r := chi.NewRouter()
	r.Get("/api/cells", h.apiCellsList)
	r.Get("/api/cells/{id}/heartbeat", h.apiCellHeartbeat)
	r.Get("/api/cells/{id}/stops", h.apiCellStops)
	r.Get("/api/cells/{id}/state", h.apiCellState)
	r.Get("/api/dashboards/{id}/cells", h.apiDashboardCells)
	read := func(path string) []byte {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	get := func(path string) any {
		var v any
		if err := json.Unmarshal(read(path), &v); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return stripIDs(v)
	}
	board := func(id int64) string { return "/api/dashboards/" + strconv.FormatInt(id, 10) + "/cells" }
	seedSince := "?since=" + now.Add(-2*time.Minute).Format(time.RFC3339)
	render := func() []byte {
		got := map[string]any{
			"cells":                  get("/api/cells"),
			"board_plant_cells":      get(board(plantBoard)),
			"board_station_cells":    get(board(stationBoard)),
			"tile_state_PLC-ALPHA":   get("/api/cells/PLC-ALPHA/state"),
			"tile_state_PLC-BRAVO":   get("/api/cells/PLC-BRAVO/state"),
			"tile_state_PLC-CHARLIE": get("/api/cells/PLC-CHARLIE/state"),
			"tile_state_PLC-RETIRED": get("/api/cells/PLC-RETIRED/state"),
			"tile_state_PLC-UNBOUND": get("/api/cells/PLC-UNBOUND/state"),
			"wall_seed_PLC-ALPHA":    get("/api/cells/PLC-ALPHA/heartbeat" + seedSince),
			"drill_PLC-ALPHA":        get("/api/cells/PLC-ALPHA/heartbeat"),
			"drill_PLC-CHARLIE":      get("/api/cells/PLC-CHARLIE/heartbeat"),
			"drill_PLC-RETIRED":      get("/api/cells/PLC-RETIRED/heartbeat"),
			"stops_PLC-ALPHA":        get("/api/cells/PLC-ALPHA/stops"),
		}
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	b := render()
	if again := render(); !bytes.Equal(b, again) {
		t.Fatalf("two reads of the same rows answered differently:\n%s\nthen:\n%s", b, again)
	}

	// What the golden says of the list, in words, so a change there names
	// itself: the listed cells are the bound, live catalog cells, station then
	// label, each named by its label, the first binding primary and the rest
	// subs.
	var listed []struct {
		CellID  string  `json:"cell_id"`
		Station string  `json:"station"`
		Primary int64   `json:"primary_process_id"`
		Subs    []int64 `json:"sub_process_ids"`
		Name    string  `json:"display_name"`
	}
	if err := json.Unmarshal(read("/api/cells"), &listed); err != nil {
		t.Fatalf("decode /api/cells: %v", err)
	}
	lines := []string{}
	for _, c := range listed {
		lines = append(lines, fmt.Sprintf("%s %s %s %d %v", c.Station, c.CellID, c.Name, c.Primary, c.Subs))
	}
	wantLines := []string{
		stnA + " PLC-ALPHA PLC-ALPHA 7 [9]",
		stnA + " PLC-BRAVO PLC-BRAVO 11 []",
		stnB + " PLC-ALPHA PLC-ALPHA 31 []",
		stnB + " PLC-CHARLIE PLC-CHARLIE 21 [22 23]",
	}
	if fmt.Sprint(lines) != fmt.Sprint(wantLines) {
		t.Errorf("/api/cells with no cell_config row:\n got  %q\n want %q", lines, wantLines)
	}

	path := filepath.Join("testdata", "cells_api_catalog_golden.json")
	if *updateCellsGolden {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), b) {
		// Name the reads that changed, and show only those.
		var wantBy, gotBy map[string]any
		if err := json.Unmarshal(want, &wantBy); err != nil {
			t.Fatalf("decode golden: %v", err)
		}
		if err := json.Unmarshal(b, &gotBy); err != nil {
			t.Fatalf("decode answer: %v", err)
		}
		changed := map[string]any{}
		for k, v := range gotBy {
			if w, ok := wantBy[k]; !ok || !reflect.DeepEqual(w, v) {
				changed[k] = v
			}
		}
		for k := range wantBy {
			if _, ok := gotBy[k]; !ok {
				changed[k] = "(no longer read)"
			}
		}
		shown, err := json.MarshalIndent(changed, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		t.Errorf("a cell surface answers differently for a catalog cell with no cell_config row. "+
			"If that is intended it is a behaviour change to name; then regenerate with "+
			"go test -tags=docker -run TestCellsAPI_CatalogCellWithNoConfigRow ./www/ -update. "+
			"The reads that changed, as they answer now:\n%s", shown)
	}
}
