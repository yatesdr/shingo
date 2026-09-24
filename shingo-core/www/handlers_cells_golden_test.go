//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

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
// seeded fixture, for a configured cell (primary 7 + sub 9) and an
// unconfigured id (station grain): /api/cells/{id}/heartbeat and /stops over
// HTTP with an explicit window, and the /state payload through the same
// service call the handler makes, at a fixed "now" (the handler reads the wall
// clock, which a golden cannot).
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
	if err := db.UpsertCellConfig(heartbeat.CellConfig{CellID: "cell-cfg", Station: "stn-p7",
		DisplayName: "Cell P7", PrimaryProcessID: 7, SubProcessIDs: []int64{9}}); err != nil {
		t.Fatalf("cell config: %v", err)
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
		if _, err := db.Exec(`INSERT INTO cell_part_events (cell_id, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id)
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
		"heartbeat_configured":   get("/api/cells/cell-cfg/heartbeat" + win),
		"heartbeat_unconfigured": get("/api/cells/stn-p7/heartbeat" + win),
		"stops_configured":       get("/api/cells/cell-cfg/stops" + win),
		"stops_unconfigured":     get("/api/cells/stn-p7/stops" + win),
		"state_configured":       state("cell-cfg"),
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
