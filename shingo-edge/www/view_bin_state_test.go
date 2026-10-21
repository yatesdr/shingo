package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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

// TestEnrichViewBinState_ProductionTileJSONIsPinned holds the Production
// page's JSON for a tile's bin (the template marshals BinState whole,
// `{{json .BinState}}`, and prints its count) byte for byte: a tote cart at a
// count of 0 standing on a base, and a full. The station view's reads moving
// (one node-bins call per board, or Core's feed) must not reach this page,
// which keeps its own read. Built twice and compared before the literal is.
func TestEnrichViewBinState_ProductionTileJSONIsPinned(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`[
			{"node_name":"PT-1","bin_id":9,"bin_label":"CART-9","bin_type_code":"TOTE-1","payload_code":"PART-A",
			 "uop_remaining":0,"delta_epoch":4,"manifest":"{\"items\":[]}","manifest_confirmed":true,"occupied":true,
			 "totes_on_cart":0,"totes_per_layer":4},
			{"node_name":"PT-2","bin_id":10,"bin_label":"BIN-10","bin_type_code":"KD-1","payload_code":"PART-B",
			 "uop_remaining":48,"manifest_confirmed":false,"occupied":true}]`)); err != nil {
			t.Errorf("stub Core write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	build := func() string {
		views := []domain.OperatorStationView{{Nodes: []domain.StationNodeView{
			{Node: domain.Node{CoreNodeName: "PT-1"}}, {Node: domain.Node{CoreNodeName: "PT-2"}},
		}}}
		enrichViewBinState(engine.NewCoreClient(srv.URL), views)
		var out []string
		for _, n := range views[0].Nodes {
			raw, err := json.Marshal(n.BinState)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			out = append(out, string(raw))
		}
		return strings.Join(out, "\n")
	}
	got := build()
	if again := build(); again != got {
		t.Fatalf("two builds differ:\n%s\n%s", got, again)
	}
	want := `{"bin_id":9,"bin_label":"CART-9","bin_type_code":"TOTE-1","payload_code":"PART-A","uop_remaining":0,"manifest":"{\"items\":[]}","manifest_confirmed":true,"occupied":true}` + "\n" +
		`{"bin_id":10,"bin_label":"BIN-10","bin_type_code":"KD-1","payload_code":"PART-B","uop_remaining":48,"manifest_confirmed":false,"occupied":true}`
	if got != want {
		t.Errorf("the Production page's bin JSON moved:\n got %s\nwant %s", got, want)
	}
}
