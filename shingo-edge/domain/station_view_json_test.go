package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStationNodeView_JSONShape pins the station's JSON for a tile: a loader
// tile carries its loader and no claim; every tile's bin carries stocked in
// place of its count; and NodeBinState itself, which the Production page
// marshals whole and prints the count from, is unchanged.
func TestStationNodeView_JSONShape(t *testing.T) {
	t.Parallel()
	bin := &NodeBinState{BinID: 4, BinLabel: "BIN-4", PayloadCode: "PART-A", UOPRemaining: 0, Occupied: true}
	claim := &NodeClaim{CoreNodeName: "N1", Role: "produce"}

	keys := func(v any) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	loaderTile := keys(StationNodeView{ActiveClaim: claim, Loader: &TileLoader{Role: "produce"}, BinState: bin})
	if _, ok := loaderTile["active_claim"]; ok {
		t.Error("a loader tile carries active_claim")
	}
	if _, ok := loaderTile["loader"]; !ok {
		t.Error("a loader tile carries no loader")
	}
	b := keys(loaderTile["bin_state"])
	if _, ok := b["uop_remaining"]; ok {
		t.Error("the station's bin carries uop_remaining")
	}
	if string(b["stocked"]) != "false" || string(b["payload_code"]) != `"PART-A"` || string(b["occupied"]) != "true" {
		t.Errorf("bin = %v, want stocked false at a count of 0, the part and occupied", b)
	}

	cell := keys(StationNodeView{ActiveClaim: claim, BinState: &NodeBinState{UOPRemaining: 3, Occupied: true}})
	if _, ok := cell["active_claim"]; !ok {
		t.Error("a cell's tile carries no active_claim")
	}
	if _, ok := cell["loader"]; ok {
		t.Error("a cell's tile carries a loader")
	}
	if string(keys(cell["bin_state"])["stocked"]) != "true" {
		t.Error("a bin counting 3 is not stocked")
	}

	// The Production page's {{json .BinState}}: the count, as it always was.
	raw, err := json.Marshal(bin)
	if err != nil {
		t.Fatalf("marshal bin: %v", err)
	}
	if !strings.Contains(string(raw), `"uop_remaining":0`) || strings.Contains(string(raw), "stocked") {
		t.Errorf("NodeBinState marshals %s; the Production page reads uop_remaining and nothing else changes", raw)
	}
}
