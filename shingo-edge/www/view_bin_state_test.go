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
	// A bare row's type code is a marker and stops here (see
	// TestEnrichViewBinState_BareCartCarriesNoMarkerCode); the bare flag travels.
	if bs.BinLabel != "BIN-5" || bs.BinTypeCode != "" || !bs.Bare || !bs.Occupied || bs.PayloadCode != "" {
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
	want := []string{"bare", "bin_id", "bin_label", "manifest_confirmed", "occupied", "uop_remaining"}
	if len(got) != len(want) {
		t.Fatalf("bin_state keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bin_state keys = %v, want %v", got, want)
		}
	}
}

// TestEnrichViewBinState_BareCartCarriesNoMarkerCode: a bare cart's bin type is
// the marker Core stamps between the stages of a two-stage unloader — Core's
// bookkeeping, never an operator's word. The tile's bin_state is what every
// Edge page reads (the Production page's bin modal prints bin_type_code), so the
// marker code stops here and only the bare flag travels. A real carrier type
// still reaches the tile.
func TestEnrichViewBinState_BareCartCarriesNoMarkerCode(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode([]map[string]any{
			{"node_name": "VB-S2", "bin_id": 7, "bin_label": "CART-7", "bin_type_code": "CART-A-BARE", "bare": true, "occupied": true},
			{"node_name": "VB-S1", "bin_id": 8, "bin_label": "CART-8", "bin_type_code": "CART-A", "occupied": true},
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	views := []domain.OperatorStationView{{Nodes: []domain.StationNodeView{
		{Node: domain.Node{CoreNodeName: "VB-S2"}}, {Node: domain.Node{CoreNodeName: "VB-S1"}},
	}}}
	enrichViewBinState(engine.NewCoreClient(srv.URL), views)

	bare, real := views[0].Nodes[0].BinState, views[0].Nodes[1].BinState
	if bare == nil || real == nil {
		t.Fatal("bin_state not set")
	}
	if !bare.Bare || bare.BinTypeCode != "" {
		t.Errorf("bare cart bin_state = %+v, want bare with no bin_type_code (the marker never reaches a page)", *bare)
	}
	if real.BinTypeCode != "CART-A" {
		t.Errorf("carrier bin_type_code = %q, want CART-A", real.BinTypeCode)
	}
}
