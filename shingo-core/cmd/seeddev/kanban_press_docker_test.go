//go:build docker

package main

import (
	"slices"
	"testing"

	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// kanban_press_docker_test.go — what seeding plants/kanban-press.yaml writes through
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
	levelsOf := func(group int64) map[string]store.MaintainLevel {
		t.Helper()
		rows, err := db.ListMaintainLevels(group)
		if err != nil {
			t.Fatalf("levels: %v", err)
		}
		out := map[string]store.MaintainLevel{}
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
		rows, err := db.ListMaintainSupports(group)
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

	k, z45, z32, k2, spare := node("PRESS-KANBAN"), node("ZONE-45"), node("ZONE-32"), node("WELD-KANBAN"), node("ZONE-45-SPARE")

	row, err := db.GetKanban(k)
	if err != nil || row == nil {
		t.Fatalf("K: %v", err)
	}
	if row.RunningCover != 4 || row.Strict || row.StationID != "edge1.line1" ||
		row.OverflowNodeID == nil || *row.OverflowNodeID != spare {
		t.Errorf("K = %+v, want cover 4, not strict, station edge1.line1, overflow ZONE-45-SPARE", row)
	}
	levels := levelsOf(k)
	for code, want := range map[string]int{"SMALL-45": 1, "BIG-45": 1, "TOTES-45": 1, "CART-32": 0} {
		if l, ok := levels[code]; !ok || l.Want != want {
			t.Errorf("K's %s = %+v, want %d", code, l, want)
		}
	}
	if ov := levels["CART-32"].OverflowNodeID; ov == nil || *ov != z32 {
		t.Errorf("K's CART-32 overflow = %v, want ZONE-32 (%d)", ov, z32)
	}
	if ov := levels["SMALL-45"].OverflowNodeID; ov != nil {
		t.Errorf("K's SMALL-45 overflow = %d, want the kanban's default", *ov)
	}
	if got := sourcesOf(k); !slices.Equal(got, []int64{z45, z32, spare}) {
		t.Errorf("K's sources = %v, want ZONE-45, ZONE-32, ZONE-45-SPARE in that rank", got)
	}
	if got := supportsOf(k); !slices.Equal(got, []string{"PLN_021", "PLN_022", "PLN_023", "PLN_024"}) {
		t.Errorf("K's supports = %v, want the four positions", got)
	}

	row2, err := db.GetKanban(k2)
	if err != nil || row2 == nil {
		t.Fatalf("K2: %v", err)
	}
	if row2.RunningCover != 1 || row2.StationID != "edge1.line1" {
		t.Errorf("K2 = %+v", row2)
	}
	if got := sourcesOf(k2); !slices.Equal(got, []int64{z45}) {
		t.Errorf("K2's sources = %v, want ZONE-45", got)
	}
	if got := supportsOf(k2); !slices.Equal(got, []string{"ALN_021"}) {
		t.Errorf("K2's supports = %v, want the weld", got)
	}

	for group, want := range map[int64]map[string]int{
		z45: {"SMALL-45": 2, "BIG-45": 2, "TOTES-45": 2},
		z32: {"CART-32": 6},
	} {
		row, err := db.GetKanban(group)
		if err != nil || row == nil {
			t.Fatalf("lender %d: %v", group, err)
		}
		if row.Strict || row.StationID != "" {
			t.Errorf("lender %+v, want not strict and no station", row)
		}
		if got := supportsOf(group); len(got) != 0 {
			t.Errorf("lender %d supports %v, want none", group, got)
		}
		if got := sourcesOf(group); len(got) != 0 {
			t.Errorf("lender %d sources %v, want none", group, got)
		}
		levels := levelsOf(group)
		if len(levels) != len(want) {
			t.Errorf("lender %d levels = %v, want %v", group, levels, want)
		}
		for code, n := range want {
			if levels[code].Want != n {
				t.Errorf("lender %d %s = %d, want %d", group, code, levels[code].Want, n)
			}
		}
	}

	// SM and the spare zone are plain groups: no kanban row, so nobody counts them.
	if sm, err := db.GetKanban(node("KP-SM")); err != nil || sm != nil {
		t.Errorf("KP-SM kanban row = %+v, %v; want a plain group", sm, err)
	}
	if sp, err := db.GetKanban(spare); err != nil || sp != nil {
		t.Errorf("ZONE-45-SPARE kanban row = %+v, %v; want a plain group", sp, err)
	}
	if ls := levelsOf(spare); len(ls) != 0 {
		t.Errorf("ZONE-45-SPARE levels = %v, want none", ls)
	}

	// Floor sizes on all four types: three of one floor, one of another.
	floors := map[string]string{}
	for _, code := range []string{"SMALL-45", "BIG-45", "TOTES-45", "CART-32"} {
		bt, err := db.GetBinTypeByCode(code)
		if err != nil || bt == nil {
			t.Fatalf("bin type %s: %v", code, err)
		}
		fs, ok := domain.BinType{WidthIn: bt.WidthIn, LengthIn: bt.LengthIn}.FloorSize()
		if !ok {
			t.Errorf("%s has no floor size: width %g, length %g", code, bt.WidthIn, bt.LengthIn)
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
	if got := sourcesOf(k); !slices.Equal(got, []int64{z45, z32, spare}) {
		t.Errorf("after a re-seed K's sources = %v", got)
	}
}
