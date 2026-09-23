package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"shingoedge/domain"
	"shingoedge/engine"
)

// view_bin_state_test.go — what enrichViewBinState copies from Core's
// node-bins row onto the tile's bin_state. The board reads bin_state and
// nothing else about the carrier, so a field missing here is a field the
// board cannot see.

func TestEnrichViewBinState_CopiesTheNodeBinsRow(t *testing.T) {
	t.Parallel()
	var reads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/telemetry/node-bins" {
			http.NotFound(w, r)
			return
		}
		reads++
		// Every key the row can carry, including bare, so the test shows
		// which of them reach bin_state.
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"node_name": "VB-W1", "bin_id": 5, "bin_label": "BIN-5", "bin_type_code": "HALF-CARRIER",
			"bare": true, "payload_code": "", "uop_remaining": 0, "occupied": true, "manifest_confirmed": false,
		}})
	}))
	t.Cleanup(srv.Close)

	views := []domain.OperatorStationView{{Nodes: []domain.StationNodeView{{Node: domain.Node{CoreNodeName: "VB-W1"}}}}}
	enrichViewBinState(engine.NewCoreClient(srv.URL), views)

	if reads != 1 {
		t.Errorf("node-bins reads = %d, want 1", reads)
	}
	bs := views[0].Nodes[0].BinState
	if bs == nil {
		t.Fatal("bin_state not set")
	}
	if bs.BinLabel != "BIN-5" || bs.BinTypeCode != "HALF-CARRIER" || !bs.Bare || !bs.Occupied || bs.PayloadCode != "" {
		t.Errorf("bin_state = %+v", *bs)
	}
	raw, err := json.Marshal(bs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	got := make([]string, 0, len(keys))
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"bare", "bin_label", "bin_type_code", "manifest_confirmed", "occupied", "uop_remaining"}
	if len(got) != len(want) {
		t.Fatalf("bin_state keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bin_state keys = %v, want %v", got, want)
		}
	}
}
