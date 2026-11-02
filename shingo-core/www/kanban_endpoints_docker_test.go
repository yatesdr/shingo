//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// kanban_endpoints_docker_test.go — the endpoints the settings modal saves
// a kanban through, and what its GET reads back. No endpoint writes a retired
// property key; the flag is gone from the wire.

func TestKanban_TheKanbanEndpoints(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	grpID, err := db.CreateNodeGroup("GRP-KB")
	testutil.MustNoErr(t, err, "create group")
	testdb.MarkGroup(t, db, grpID)
	for i := 1; i <= 4; i++ {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{
			Name: fmt.Sprintf("GRP-KB-P%d", i), ParentID: &grpID, Enabled: true,
		}), "create position")
	}
	z45, err := db.CreateNodeGroup("GRP-KB-Z45")
	testutil.MustNoErr(t, err, "z45")
	testdb.MarkGroup(t, db, z45)
	z32, err := db.CreateNodeGroup("GRP-KB-Z32")
	testutil.MustNoErr(t, err, "z32")
	testdb.MarkGroup(t, db, z32)
	small := mintBinType(t, db, "KB-SMALL").ID

	post := func(fn http.HandlerFunc, path string, body map[string]any, wantCode int) map[string]any {
		t.Helper()
		body["group_node_id"] = grpID
		rec := postJSON(t, fn, path, body)
		if rec.Code != wantCode {
			t.Fatalf("%s: status %d, want %d; body=%s", path, rec.Code, wantCode, rec.Body.String())
		}
		var out map[string]any
		testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode "+path)
		return out
	}

	post(h.apiKanbanSettingsSet, "/api/nodes/kanban/settings", map[string]any{
		"strict_sourcing": true, "maintenance_station": "edge.1", "overflow_destination": "GRP-KB-Z45"}, http.StatusOK)
	post(h.apiKanbanLevelSet, "/api/nodes/kanban/level", map[string]any{"bin_type_id": small, "want": 1}, http.StatusOK)
	post(h.apiKanbanSourcesSet, "/api/nodes/kanban/sources",
		map[string]any{"source_node_ids": []int64{z32, z45}}, http.StatusOK)

	rec := getPlain(t, h.apiKanban, fmt.Sprintf("/api/nodes/kanban?id=%d", grpID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: status %d", rec.Code)
	}
	var cfg struct {
		Strict   bool                 `json:"strict_sourcing"`
		Station  string               `json:"maintenance_station"`
		Overflow string               `json:"overflow_destination"`
		Levels   []store.KanbanLevel  `json:"levels"`
		Sources  []store.KanbanSource `json:"sources"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &cfg), "decode")
	if !cfg.Strict || cfg.Station != "edge.1" || cfg.Overflow != "" {
		t.Errorf("settings = %+v", cfg)
	}
	if len(cfg.Levels) != 1 {
		t.Errorf("levels = %+v", cfg.Levels)
	}
	if len(cfg.Sources) != 2 || cfg.Sources[0].SourceNodeName != "GRP-KB-Z32" || cfg.Sources[1].Rank != 2 {
		t.Errorf("sources = %+v", cfg.Sources)
	}
	for _, key := range []string{"strict_sourcing", "maintenance_station", "overflow_destination"} {
		if got := db.GetNodeProperty(grpID, key); got != "" {
			t.Errorf("property %s = %q on a kanban", key, got)
		}
	}
	rows, err := db.ListEntityAudit("node", grpID)
	testutil.MustNoErr(t, err, "audit")
	if len(rows) != 6 {
		t.Errorf("audit rows = %d, want 6: strict, station, level, its pool, two sources", len(rows))
	}

	// A refusal is a 409 with the rule's sentence, and writes nothing.
	out := post(h.apiKanbanSourcesSet, "/api/nodes/kanban/sources",
		map[string]any{"source_node_ids": []int64{grpID}}, http.StatusConflict)
	if out["error"] == nil {
		t.Errorf("no sentence: %+v", out)
	}
	if got, err := db.ListKanbanSources(grpID); err != nil {
		t.Fatal(err)
	} else if len(got) != 2 {
		t.Errorf("a refused list was written: %+v", got)
	}

	// A malformed request is the request's fault.
	if rec := postJSON(t, h.apiKanbanLevelSet, "/api/nodes/kanban/level",
		map[string]any{"group_node_id": grpID}); rec.Code != http.StatusBadRequest {
		t.Errorf("no bin type: status %d", rec.Code)
	}
}
