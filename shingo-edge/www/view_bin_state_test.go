package www

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine"
)

// view_bin_state_test.go — what enrichViewBinState copies from the bins
// table's row onto the Production page's bin_state. The page reads bin_state
// and nothing else about the carrier, so a field missing here is a field the
// page cannot see.

// heldBins is a bins table held now, confirmed now, holding rows.
func heldBins(rows ...protocol.NodeBinsRow) engine.NodeBinsState {
	st := engine.NodeBinsState{Rows: map[string]protocol.NodeBinsRow{}, Held: true,
		ReceivedAt: time.Now(), ConfirmedAt: time.Now()}
	for _, r := range rows {
		st.Rows[r.Node] = r
	}
	return st
}

func TestEnrichViewBinState_CopiesTheNodeBinsRow(t *testing.T) {
	t.Parallel()
	views := []domain.OperatorStationView{{Nodes: []domain.StationNodeView{{Node: domain.Node{CoreNodeName: "VB-W1"}}}}}
	enrichViewBinState(heldBins(protocol.NodeBinsRow{
		Node: "VB-W1", State: protocol.BinsStateOccupied, BinID: 5, BinLabel: "BIN-5", BinTypeCode: "HALF-CARRIER",
		Bare: true, Occupied: true, Epoch: 3,
	}), time.Now(), views)

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
	want := []string{"bare", "bin_id", "bin_label", "manifest_confirmed", "occupied", "stocked", "uop_remaining"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("bin_state keys = %v, want %v", got, want)
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
	views := []domain.OperatorStationView{{Nodes: []domain.StationNodeView{
		{Node: domain.Node{CoreNodeName: "VB-S2"}}, {Node: domain.Node{CoreNodeName: "VB-S1"}},
	}}}
	enrichViewBinState(heldBins(
		protocol.NodeBinsRow{Node: "VB-S2", BinID: 7, BinLabel: "CART-7", BinTypeCode: "CART-A-BARE", Bare: true, Occupied: true},
		protocol.NodeBinsRow{Node: "VB-S1", BinID: 8, BinLabel: "CART-8", BinTypeCode: "CART-A", Occupied: true},
	), time.Now(), views)

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
// `{{json .BinState}}`) byte for byte: a tote cart with no parts, a partial
// and a held partial. CHANGED from the HTTP read it replaced: no count
// (uop_remaining is 0 but for a held partial's declared count), stocked and
// partial in its place. Built twice and compared before the literal is.
func TestEnrichViewBinState_ProductionTileJSONIsPinned(t *testing.T) {
	t.Parallel()
	manifest := `{"items":[]}`
	held := heldBins(
		protocol.NodeBinsRow{Node: "PT-1", State: protocol.BinsStateOccupied, BinID: 9, BinLabel: "CART-9", BinTypeCode: "TOTE-1",
			PayloadCode: "PART-A", Epoch: 4, Manifest: &manifest, ManifestConfirmed: true, Occupied: true},
		protocol.NodeBinsRow{Node: "PT-2", State: protocol.BinsStateOccupied, BinID: 10, BinLabel: "BIN-10", BinTypeCode: "KD-1",
			PayloadCode: "PART-B", Occupied: true, Stocked: true, Partial: true, UOPCapacity: 60},
		protocol.NodeBinsRow{Node: "PT-3", State: protocol.BinsStateOccupied, BinID: 11, BinLabel: "BIN-11", BinTypeCode: "KD-1",
			PayloadCode: "PART-B", Occupied: true, Stocked: true, Partial: true, UOPCapacity: 60,
			HeldSince: "2026-10-19T08:00:00Z", HeldFor: "LINE-1", HeldUOP: 12},
	)
	build := func() string {
		views := []domain.OperatorStationView{{Nodes: []domain.StationNodeView{
			{Node: domain.Node{CoreNodeName: "PT-1"}}, {Node: domain.Node{CoreNodeName: "PT-2"}}, {Node: domain.Node{CoreNodeName: "PT-3"}},
		}}}
		enrichViewBinState(held, time.Now(), views)
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
	want := `{"bin_id":9,"bin_label":"CART-9","bin_type_code":"TOTE-1","payload_code":"PART-A","uop_remaining":0,"manifest":"{\"items\":[]}","manifest_confirmed":true,"occupied":true,"stocked":false}` + "\n" +
		`{"bin_id":10,"bin_label":"BIN-10","bin_type_code":"KD-1","payload_code":"PART-B","uop_remaining":0,"manifest_confirmed":false,"occupied":true,"uop_capacity":60,"stocked":true,"partial":true}` + "\n" +
		`{"bin_id":11,"bin_label":"BIN-11","bin_type_code":"KD-1","payload_code":"PART-B","uop_remaining":12,"manifest_confirmed":false,"occupied":true,"uop_capacity":60,"held_since":"2026-10-19T08:00:00Z","held_for":"LINE-1","stocked":true,"partial":true}`
	if got != want {
		t.Errorf("the Production page's bin JSON moved:\n got %s\nwant %s", got, want)
	}
}

// TestEnrichViewBinState_OnlyACurrentTable: with no table held, or one Core
// has not confirmed for longer than the screens' limit, no tile gets a
// bin_state (the page shows what it showed when Core could not be read); a
// name the table holds no row for gets none either.
func TestEnrichViewBinState_OnlyACurrentTable(t *testing.T) {
	t.Parallel()
	tiles := func() []domain.OperatorStationView {
		return []domain.OperatorStationView{{Nodes: []domain.StationNodeView{
			{Node: domain.Node{CoreNodeName: "OT-1"}}, {Node: domain.Node{CoreNodeName: "OT-UNWATCHED"}},
		}}}
	}
	row := protocol.NodeBinsRow{Node: "OT-1", State: protocol.BinsStateOccupied, BinID: 3, Occupied: true}

	views := tiles()
	enrichViewBinState(engine.NodeBinsState{}, time.Now(), views)
	if views[0].Nodes[0].BinState != nil {
		t.Error("nothing held: a tile got a bin_state")
	}
	enrichViewBinState(engine.NodeBinsState{}, time.Now(), nil)

	views = tiles()
	enrichViewBinState(heldBins(row), time.Now().Add(engine.FeedAsOfAfter+time.Second), views)
	if views[0].Nodes[0].BinState != nil {
		t.Error("a table older than the screens' limit: a tile got a bin_state")
	}

	views = tiles()
	enrichViewBinState(heldBins(row), time.Now(), views)
	if bs := views[0].Nodes[0].BinState; bs == nil || bs.BinID != 3 {
		t.Errorf("a current table: bin_state = %+v, want bin 3", bs)
	}
	if views[0].Nodes[1].BinState != nil {
		t.Error("a name with no row got a bin_state")
	}
}
