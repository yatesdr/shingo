//go:build docker

package main

import (
	"slices"
	"testing"

	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// kanban_press_docker_test.go — what seeding the master's [kanban] section writes through
// store.SaveKanban: the rows the runs stand on, read back as a reader reads them.
//
// The fifth part's sentence on the card is the picture's (NothingProcesses), and it
// reads the Edge's claims mirror, which only a running Edge publishes; here the part's
// half of it is pinned: seeded, and with no carrier.
func TestSeedKanbanPress_CoreHoldsTheKanbans(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	plant := kanbanPressPlant(t)
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore: %v", err)
	}

	node := func(name string) int64 {
		t.Helper()
		n, err := db.GetNodeByName(name)
		if err != nil || n == nil {
			t.Fatalf("node %s: %v", name, err)
		}
		return n.ID
	}
	levelsOf := func(group int64) map[string]store.KanbanLevel {
		t.Helper()
		rows, err := db.ListKanbanLevels(group)
		if err != nil {
			t.Fatalf("levels: %v", err)
		}
		out := map[string]store.KanbanLevel{}
		for _, l := range rows {
			out[l.BinTypeCode] = l
		}
		return out
	}
	sourcesOf := func(group int64) []int64 {
		t.Helper()
		rows, err := db.ListKanbanSources(group)
		if err != nil {
			t.Fatalf("sources: %v", err)
		}
		var out []int64
		for _, s := range rows {
			out = append(out, s.SourceNodeID)
		}
		return out
	}
	supportsOf := func(group int64) []string {
		t.Helper()
		rows, err := db.ListKanbanSupports(group)
		if err != nil {
			t.Fatalf("supports: %v", err)
		}
		var out []string
		for _, s := range rows {
			out = append(out, s.ProcessNodeName)
		}
		slices.Sort(out)
		return out
	}

	k, k2 := node("PRESS-KANBAN"), node("WELD-KANBAN")

	row, err := db.GetKanban(k)
	if err != nil || row == nil {
		t.Fatalf("K: %v", err)
	}
	if row.Strict || row.StationID != "edge1.line1" {
		t.Errorf("K = %+v, want not strict, station edge1.line1", row)
	}
	pools, err := db.ListKanbanPools(k)
	if err != nil {
		t.Fatal(err)
	}
	fam := func(code string) int64 {
		bt, err := db.GetBinTypeByCode(code)
		if err != nil || bt == nil {
			t.Fatalf("bin type %s: %v", code, err)
		}
		return bt.FamilyID
	}
	if pools[fam("SMALL-45")] != 10 || pools[fam("CART-32")] != 6 || fam("SMALL-45") != fam("BIG-45") || fam("SMALL-45") != fam("TOTES-45") {
		t.Errorf("K's pools = %v, want 45x48 10 and CART-32 6", pools)
	}
	levels := levelsOf(k)
	for _, code := range []string{"SMALL-45", "BIG-45", "TOTES-45", "CART-32"} {
		if l, ok := levels[code]; !ok || l.Want != 2 {
			t.Errorf("K's %s = %+v, want 2", code, l)
		}
	}
	if got := sourcesOf(k); len(got) != 0 {
		t.Errorf("K's sources = %v, want none: its keeper searches plant-wide", got)
	}
	if got := supportsOf(k); !slices.Equal(got, []string{"PLN_021", "PLN_022", "PLN_023", "PLN_024"}) {
		t.Errorf("K's supports = %v, want the four positions", got)
	}

	row2, err := db.GetKanban(k2)
	if err != nil || row2 == nil {
		t.Fatalf("K2: %v", err)
	}
	if row2.StationID != "edge1.line1" {
		t.Errorf("K2 = %+v, want station edge1.line1", row2)
	}
	if got := sourcesOf(k2); len(got) != 0 {
		t.Errorf("K2's sources = %v, want none", got)
	}
	if got := supportsOf(k2); !slices.Equal(got, []string{"ALN_021"}) {
		t.Errorf("K2's supports = %v, want the weld", got)
	}

	// SM is a plain group: no kanban row, so nobody counts it.
	if sm, err := db.GetKanban(node("KP-SM")); err != nil || sm != nil {
		t.Errorf("KP-SM kanban row = %+v, %v; want a plain group", sm, err)
	}

	// The seed writes each slot's allowed types, per type, as the file's admits say.
	for slot, types := range rigSlotTypes() {
		got, err := db.ListBinTypesForNode(node(slot))
		if err != nil {
			t.Fatalf("types of %s: %v", slot, err)
		}
		var codes []string
		for _, bt := range got {
			codes = append(codes, bt.Code)
		}
		slices.Sort(codes)
		want := slices.Clone(types)
		slices.Sort(want)
		if !slices.Equal(codes, want) || db.GetNodeProperty(node(slot), "bin_type_mode") != "specific" {
			t.Errorf("the seed wrote %v on %s, want %v", codes, slot, want)
		}
	}

	// Floor sizes on all four types: three of one floor, one of another.
	floors := map[string]string{}
	for _, code := range []string{"SMALL-45", "BIG-45", "TOTES-45", "CART-32"} {
		bt, err := db.GetBinTypeByCode(code)
		if err != nil || bt == nil {
			t.Fatalf("bin type %s: %v", code, err)
		}
		fs, ok := domain.BinType{WidthIn: bt.WidthIn, LengthIn: bt.LengthIn}.Footprint()
		if !ok {
			t.Errorf("%s has no footprint: width %g, length %g", code, bt.WidthIn, bt.LengthIn)
		}
		floors[code] = fs.String()
	}
	if floors["SMALL-45"] != "45x48" || floors["BIG-45"] != "45x48" || floors["TOTES-45"] != "45x48" || floors["CART-32"] != "30x32" {
		t.Errorf("floors = %v", floors)
	}

	// One carrier row per part, and none on the part that says so.
	for part, want := range map[string]int{"KP-SM-L": 1, "KP-SM-R": 1, "KP-BIG": 1, "KP-TOTE": 1, "KP-32": 1, "KW-PART": 1, "KP-UNTYPED": 0} {
		p, err := db.GetPayloadByCode(part)
		if err != nil || p == nil {
			t.Fatalf("part %s: %v", part, err)
		}
		rows, err := db.ListBinTypesForPayload(p.ID)
		if err != nil {
			t.Fatalf("carriers of %s: %v", part, err)
		}
		if len(rows) != want {
			t.Errorf("%s has %d payload_bin_types rows, want %d", part, len(rows), want)
		}
	}

	// The unloader: one shared loader over its four windows, outbound K.
	l, err := db.GetLoaderByName("KP-UNLOADER", "consume")
	if err != nil || l == nil {
		t.Fatalf("KP-UNLOADER: %v", err)
	}
	if l.OutboundDest != "PRESS-KANBAN" || l.InboundSource != "" || l.Layout != "shared_window" {
		t.Errorf("KP-UNLOADER = %+v", l)
	}
	homes, err := db.ListLoaderHomes(l.ID)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(homes) != 4 {
		t.Errorf("KP-UNLOADER has %d windows, want 4", len(homes))
	}
	parts, err := db.ListLoaderPayloads(l.ID)
	if err != nil {
		t.Fatalf("parts: %v", err)
	}
	if len(parts) != 6 {
		t.Errorf("KP-UNLOADER lists %d parts, want the press's six", len(parts))
	}

	// A re-seed changes nothing it should not.
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := sourcesOf(k); len(got) != 0 {
		t.Errorf("after a re-seed K's sources = %v", got)
	}
}
