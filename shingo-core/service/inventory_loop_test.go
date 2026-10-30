package service

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/inventory"
	"shingocore/store/loaders"
	"shingocore/store/orders"
)

// binding is a monitored (position, part) as LoopBindings reads it.
func binding(node, payload, layout string, threshold, pack, windows int) inventory.Binding {
	return inventory.Binding{CoreNodeName: node, PayloadCode: payload, Threshold: threshold, LoaderID: 1,
		LoaderName: "L", Layout: layout, PositionNodeID: 9, Pack: pack, Windows: windows, Members: node, CompletedL1: 1}
}

func rowFor(t *testing.T, v LoopView, node string) LoopRow {
	t.Helper()
	for _, r := range v.Rows {
		if r.Node == node {
			return r
		}
	}
	t.Fatalf("no row at %s in %+v", node, v.Rows)
	return LoopRow{}
}

// TestBuildLoopView_ChipsAndBelow pins the owner's chip rule (2026-10-29): a
// one-bin dedicated home at a UoP threshold of a pack or more refills when it
// empties and is not read as below threshold; a threshold over what a
// multi-bin home (a shared loader's windows) can hold is above home capacity,
// not below either; a home under a pack with less in the loop is below.
func TestBuildLoopView_ChipsAndBelow(t *testing.T) {
	t.Parallel()
	v := BuildLoopView(LoopInputsRead{
		Bindings: []inventory.Binding{
			binding("SMN_018", "P-FULL", loaders.LayoutDedicatedPositions, 251, 250, 1), // threshold over a pack
			binding("SMN_035", "P-EQ", loaders.LayoutDedicatedPositions, 4500, 4500, 1), // threshold = a pack
			binding("SMN_023", "P-LOW", loaders.LayoutDedicatedPositions, 120, 2000, 1), // under a pack, 100 in loop
			binding("PLK_W1", "P-WIN", loaders.LayoutSharedWindow, 90, 40, 2),           // 90 > 2 x 40
			binding("PLK_X1", "P-OK", loaders.LayoutSharedWindow, 40, 40, 1),            // 40 = 1 x 40: not over
			binding("SMN_099", "P-NOPACK", loaders.LayoutDedicatedPositions, 500, 0, 1), // no pack stated
		},
		Sums: []PayloadSystemUOP{
			{PayloadCode: "P-FULL", TotalUOP: 250}, {PayloadCode: "P-EQ", TotalUOP: 3096},
			{PayloadCode: "P-LOW", TotalUOP: 100}, {PayloadCode: "P-WIN", TotalUOP: 30},
			{PayloadCode: "P-OK", TotalUOP: 10}, {PayloadCode: "P-NOPACK", TotalUOP: 10},
		},
	})
	for node, want := range map[string]string{
		"SMN_018": LoopRefillsWhenEmpty, "SMN_035": LoopRefillsWhenEmpty, "SMN_023": LoopBelow,
		"PLK_W1": LoopAboveHomeCapacity, "PLK_X1": LoopBelow, "SMN_099": LoopBelow,
	} {
		if got := rowFor(t, v, node).State; got != want {
			t.Errorf("%s: state %q, want %q", node, got, want)
		}
	}
	if r := rowFor(t, v, "PLK_W1"); r.HomeBins != 2 || r.HomeCapacity != 80 {
		t.Errorf("shared loader of 2 windows x 40: home %d bins, capacity %d; want 2 and 80", r.HomeBins, r.HomeCapacity)
	}
}

// TestBuildLoopView_TimesAndSuggestion pins what the times and the suggestion
// are made of: cover (time to empty) is in-loop UoP x the cycle from the median
// measured rate, lead time (refill takes) the calculator's four inputs summed
// (L2 transit 0), and the suggestion CalculateThresholds over the same inputs.
func TestBuildLoopView_TimesAndSuggestion(t *testing.T) {
	t.Parallel()
	in := LoopInputsRead{
		Bindings: []inventory.Binding{
			binding("A", "P-CLEAR", loaders.LayoutDedicatedPositions, 100, 500, 1),
			binding("B", "P-MARGIN", loaders.LayoutDedicatedPositions, 100, 500, 1),
			binding("C", "P-SHORT", loaders.LayoutDedicatedPositions, 100, 500, 1),
			binding("D", "P-NORATE", loaders.LayoutDedicatedPositions, 100, 500, 1),
		},
		Sums: []PayloadSystemUOP{
			{PayloadCode: "P-CLEAR", TotalUOP: 200}, {PayloadCode: "P-MARGIN", TotalUOP: 60},
			{PayloadCode: "P-SHORT", TotalUOP: 20}, {PayloadCode: "P-NORATE", TotalUOP: 20},
		},
		Leads: map[string]orders.LeadTimeInputs{
			// lead 60 + 120 + 300 = 480 s; rate 0.1/s -> cycle 10 s.
			"P-CLEAR":  {L1QueueSeconds: 60, L1TransitSeconds: 120, L2LoadSeconds: 300, MarketToCellSeconds: 90, SamplesL1: 3, SamplesRetrieve: 3, RatePerSec: 0.1},
			"P-MARGIN": {L1QueueSeconds: 60, L1TransitSeconds: 120, L2LoadSeconds: 300, RatePerSec: 0.1},
			"P-SHORT":  {L1QueueSeconds: 60, L1TransitSeconds: 120, L2LoadSeconds: 300, RatePerSec: 0.1},
			"P-NORATE": {L1QueueSeconds: 60, L1TransitSeconds: 120, L2LoadSeconds: 300},
		},
	}
	v := BuildLoopView(in)
	clear := rowFor(t, v, "A")
	if clear.CycleSeconds == nil || *clear.CycleSeconds != 10 || clear.CoverSeconds == nil || *clear.CoverSeconds != 2000 ||
		clear.LeadSeconds == nil || *clear.LeadSeconds != 480 {
		t.Fatalf("A: cycle/cover/lead = %v/%v/%v, want 10/2000/480",
			deref(clear.CycleSeconds), deref(clear.CoverSeconds), deref(clear.LeadSeconds))
	}
	for node, want := range map[string]float64{"B": 600, "C": 200} {
		if got := rowFor(t, v, node).CoverSeconds; got == nil || *got != want {
			t.Errorf("%s: cover %v, want %v", node, deref(got), want)
		}
	}
	// ceil(480 / 10 x 1.5) = 72, and LOW: L2 coverage is always 0.
	if clear.Suggested == nil || *clear.Suggested != 72 || clear.Confidence != "LOW" {
		t.Errorf("A: suggestion %v %q, want 72 LOW", clear.Suggested, clear.Confidence)
	}
	norate := rowFor(t, v, "D")
	if norate.CoverSeconds != nil || norate.Suggested != nil || norate.LeadSeconds == nil {
		t.Errorf("D (no measured rate): cover %v, suggestion %v, lead %v; want no cover, no suggestion, the lead time",
			norate.CoverSeconds, norate.Suggested, norate.LeadSeconds)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestBuildLoopView_DoubtOffHomeAndLines pins the doubt line (a bin below zero
// and a bin nobody confirmed are named; the bin filling at a produce seat is
// not: its count climbs from the Edge's ticks until it is released), the bins
// named off their home, the lines a loader row is grouped under, a produce
// line's row (in bin, its cycle from what it made, the station), and that a
// loader's window is a position, never a line.
func TestBuildLoopView_DoubtOffHomeAndLines(t *testing.T) {
	t.Parallel()
	v := BuildLoopView(LoopInputsRead{
		Bindings: []inventory.Binding{binding("SMN_025", "P-OFF", loaders.LayoutDedicatedPositions, 100, 2000, 1)},
		Sums:     []PayloadSystemUOP{{PayloadCode: "P-OFF", TotalUOP: 3248}},
		Bins: []inventory.LoopBin{
			{ID: 1, Label: "B1", Node: "SMN_004", PayloadCode: "P-OFF", UOP: 3248, Confirmed: true},
			{ID: 2, Label: "B2", Node: "ALN_004", PayloadCode: "P-NEG", UOP: -407, Confirmed: true},
			{ID: 3, Label: "B3", Node: "ALN_003", PayloadCode: "P-UNC", UOP: 5, Confirmed: false},
			{ID: 4, Label: "B4", Node: "PLN_001", PayloadCode: "P-FG", UOP: 18, Confirmed: false}, // filling at its produce seat: not doubtful
		},
		Claims: []inventory.LoopClaim{
			{ProcessID: "SNF3", CoreNodeName: "ALN_011", PayloadCode: "P-OFF", Role: loaders.RoleConsume},
			{ProcessID: "SNF2", CoreNodeName: "ALN_012", PayloadCode: "P-OFF", Role: loaders.RoleConsume},
			{ProcessID: "PRESS-1", CoreNodeName: "PLN_001", PayloadCode: "P-FG", Role: loaders.RoleProduce, Pack: 30,
				ReorderPoint: 4, Station: "Press Line"},
			{ProcessID: "LOADERS", CoreNodeName: "PLK_W1", PayloadCode: "P-WIN", Role: loaders.RoleProduce, LoaderMember: true},
		},
		LineRates: map[string]float64{"line|PRESS-1|PLN_001|P-FG|produce": 0.05},
	})
	if len(v.Doubt) != 2 || v.Doubt[0].Label != "B3" || v.Doubt[0].Reason != "unconfirmed" ||
		v.Doubt[1].Label != "B2" || v.Doubt[1].Reason != "below_zero" {
		t.Errorf("doubt = %+v, want B3 unconfirmed (ALN_003) and B2 below_zero (ALN_004); B4 is filling at PLN_001", v.Doubt)
	}
	off := rowFor(t, v, "SMN_025")
	if len(off.OffHome) != 1 || off.OffHome[0].Node != "SMN_004" || off.OffHome[0].Bins != 1 {
		t.Errorf("off home = %+v, want 1 bin at SMN_004", off.OffHome)
	}
	if off.Group != "SNF2, SNF3" {
		t.Errorf("group %q, want SNF2, SNF3", off.Group)
	}
	line := rowFor(t, v, "PLN_001")
	if line.Kind != "line" || line.Role != loaders.RoleProduce || line.InBin == nil || *line.InBin != 18 ||
		line.ReorderPoint != 4 || line.CycleSeconds == nil || *line.CycleSeconds != 20 ||
		line.CoverSeconds == nil || *line.CoverSeconds != 240 || line.Station != "Press Line" {
		t.Errorf("produce line row = %+v, want 18 in the bin, a 20 s cycle, 12 left to fill = 240 s", line)
	}
	for _, r := range v.Rows {
		if r.Node == "PLK_W1" {
			t.Error("a loader window's produce claim is listed as a line")
		}
	}
}

// TestBuildLoopView_Lines pins the Lines table's row for each role (owner,
// correction 2, 2026-10-30): one row per claim of a running style, consume and
// produce; time left until the bin is empty (consume) or full (produce) at the
// node's tick rate, absent with no rate; past the threshold when a consume bin
// is at or under its cell reorder point, or a produce bin at or over the level
// its cell decides at (protocol.ClaimDemandLevel: the reorder point when it is
// under the pack, else the pack) or over its pack. No bin or an empty one at a
// consume spot, and a full one at a produce spot, is nothing left whatever the
// rate (SPR 2026-10-30: SNF3's full bin read "?" while it waited for its swap,
// because a full bin stops the line); no bin at a produce spot is nothing to
// fill.
func TestBuildLoopView_Lines(t *testing.T) {
	t.Parallel()
	rates := map[string]float64{}
	claim := func(node, role string, pack, rp int, rate float64) inventory.LoopClaim {
		c := inventory.LoopClaim{ProcessID: "SNF3", CoreNodeName: node, PayloadCode: "P-" + node, Role: role,
			Pack: pack, ReorderPoint: rp}
		rates[lineKey(c)] = rate
		return c
	}
	bin := func(node string, uop int) inventory.LoopBin {
		return inventory.LoopBin{ID: int64(len(node)), Label: node, Node: node, PayloadCode: "P-" + node, UOP: uop, Confirmed: true}
	}
	v := BuildLoopView(LoopInputsRead{
		Bins: []inventory.LoopBin{
			bin("C-OK", 100), bin("C-AT", 20), bin("C-NORATE", 5), bin("C-EMPTY", 0),
			bin("P-OK", 50), bin("P-EARLY", 120), bin("P-FULL", 132), bin("P-OVER", 140), bin("P-STOPPED", 132),
		},
		Claims: []inventory.LoopClaim{
			claim("C-OK", loaders.RoleConsume, 132, 20, 0.1),     // 100 at 10 s each: 1000 s, over its RP
			claim("C-AT", loaders.RoleConsume, 132, 20, 0.1),     // at its RP: beyond
			claim("C-NORATE", loaders.RoleConsume, 132, 0, 0),    // no rate: no time left; 5 > RP 0
			claim("C-NONE", loaders.RoleConsume, 132, 0, 0.1),    // no bin: nothing left, beyond even at RP 0
			claim("C-EMPTY", loaders.RoleConsume, 132, 0, 0),     // an empty bin, no rate: nothing left
			claim("P-OK", loaders.RoleProduce, 132, 0, 0.1),      // RP 0 decides at the pack: 50 is short of it
			claim("P-EARLY", loaders.RoleProduce, 132, 110, 0.1), // RP 110 under the pack: 120 is past it
			claim("P-FULL", loaders.RoleProduce, 132, 0, 0.1),    // at the pack
			claim("P-OVER", loaders.RoleProduce, 132, 200, 0.1),  // RP over the pack decides at the pack; and over the pack
			claim("P-NONE", loaders.RoleProduce, 132, 0, 0.1),    // no bin: nothing to fill, not beyond
			claim("P-STOPPED", loaders.RoleProduce, 132, 0, 0),   // full, no rate: nothing left
		},
		LineRates: rates,
	})
	for _, c := range []struct {
		node   string
		inBin  any
		cover  any
		beyond bool
	}{
		{"C-OK", 100, 1000.0, false},
		{"C-AT", 20, 200.0, true},
		{"C-NORATE", 5, nil, false},
		{"C-NONE", nil, 0.0, true},
		{"C-EMPTY", 0, 0.0, true},
		{"P-OK", 50, 820.0, false},
		{"P-EARLY", 120, 120.0, true},
		{"P-FULL", 132, 0.0, true},
		{"P-OVER", 140, 0.0, true},
		{"P-NONE", nil, nil, false},
		{"P-STOPPED", 132, 0.0, true},
	} {
		r := rowFor(t, v, c.node)
		var inBin any
		if r.InBin != nil {
			inBin = *r.InBin
		}
		if r.Kind != "line" || inBin != c.inBin || deref(r.CoverSeconds) != c.cover || r.Beyond != c.beyond || r.Look != c.beyond {
			t.Errorf("%s: in bin %v, time left %v, beyond %v, look %v; want %v, %v, %v, %v",
				c.node, inBin, deref(r.CoverSeconds), r.Beyond, r.Look, c.inBin, c.cover, c.beyond, c.beyond)
		}
	}
}

// TestBuildLoopView_LineOnTheWay pins a line's On the way: the live orders not
// yet delivered that set a bin down at its node, by the one spelling
// (protocol.OrderPlacesBinAt). A plain order by its delivery node; a complex
// order by its steps, so a swap's final drop elsewhere still counts at the line
// and a lift that sets nothing down there does not. A consume spot counts fulls
// of its part; a produce spot counts the empties coming whatever they are for.
func TestBuildLoopView_LineOnTheWay(t *testing.T) {
	t.Parallel()
	// A one-robot swap lifts the line's bin, puts it away, fetches the other
	// and sets it down at the line last.
	swapAt := func(node string) string {
		return `[{"action":"pickup","node":"` + node + `"},{"action":"dropoff","node":"SMN_002"},` +
			`{"action":"pickup","node":"SMN_001"},{"action":"dropoff","node":"` + node + `"}]`
	}
	v := BuildLoopView(LoopInputsRead{
		Claims: []inventory.LoopClaim{
			{ProcessID: "SNF3", CoreNodeName: "ALN_011", PayloadCode: "P-IN", Role: loaders.RoleConsume, Pack: 132},
			{ProcessID: "SNF3", CoreNodeName: "ALN_012", PayloadCode: "P-OUT", Role: loaders.RoleProduce, Pack: 2400},
			{ProcessID: "SNF3", CoreNodeName: "ALN_013", PayloadCode: "P-IDLE", Role: loaders.RoleProduce, Pack: 2400},
		},
		Live: []inventory.LiveOrder{
			{OrderType: protocol.OrderTypeRetrieve, DeliveryNode: "ALN_011", PayloadCode: "P-IN"},    // a full: counts
			{OrderType: protocol.OrderTypeRetrieve, DeliveryNode: "ALN_011", PayloadCode: "P-OTHER"}, // another part: not at this row
			{OrderType: protocol.OrderTypeComplex, StepsJSON: swapAt("ALN_011"), DeliveryNode: "SMN_002", PayloadCode: "P-IN"},
			{OrderType: protocol.OrderTypeComplex, StepsJSON: swapAt("ALN_012"), DeliveryNode: "SMN_002", PayloadCode: "P-OUT"},
			{OrderType: protocol.OrderTypeRetrieveEmpty, DeliveryNode: "ALN_012"}, // an empty: counts
			{OrderType: protocol.OrderTypeComplex, PayloadCode: "P-OUT", DeliveryNode: "SMN_002", // the full's release
				StepsJSON: `[{"action":"pickup","node":"ALN_012"},{"action":"dropoff","node":"SMN_002"}]`},
			{OrderType: protocol.OrderTypeComplex, StepsJSON: `not json`, DeliveryNode: "ALN_012"}, // nothing says where
		},
	})
	for node, want := range map[string]int{"ALN_011": 2, "ALN_012": 2, "ALN_013": 0} {
		r := rowFor(t, v, node)
		if r.Coming != want || r.Look != (want > 0 || r.Beyond) {
			t.Errorf("%s: on the way %d, look %v; want %d", node, r.Coming, r.Look, want)
		}
	}
}

// TestBuildLoopView_LookRule pins the default view (owner, correction 2): a
// position shows when its in-loop UoP is at or under its UoP threshold (a home
// that refills when it empties: when that home is empty), its threshold is
// above home capacity, its part's count is in doubt, or a bin is on the way; a
// line when it is past its threshold, the bin at it is in doubt, or a bin is on
// the way. Everything else folds. The second plant is the one
// where nothing qualifies: every row healthy, so the page shows "All N
// positions healthy" and never an empty table.
func TestBuildLoopView_LookRule(t *testing.T) {
	t.Parallel()
	coming := binding("SMN_004", "P-COMING", loaders.LayoutDedicatedPositions, 100, 500, 1)
	coming.Coming = 1
	v := BuildLoopView(LoopInputsRead{
		Bindings: []inventory.Binding{
			binding("SMN_001", "P-AT", loaders.LayoutDedicatedPositions, 100, 500, 1),      // 100 <= 100
			binding("SMN_002", "P-HEALTHY", loaders.LayoutDedicatedPositions, 100, 500, 1), // 400 > 100
			binding("PLK_W1", "P-CAP", loaders.LayoutSharedWindow, 90, 40, 2),              // above home capacity
			binding("SMN_003", "P-DOUBT", loaders.LayoutDedicatedPositions, 100, 500, 1),   // a bin below zero
			coming,
		},
		Sums: []PayloadSystemUOP{
			{PayloadCode: "P-AT", TotalUOP: 100}, {PayloadCode: "P-HEALTHY", TotalUOP: 400},
			{PayloadCode: "P-CAP", TotalUOP: 200}, {PayloadCode: "P-DOUBT", TotalUOP: 400},
			{PayloadCode: "P-COMING", TotalUOP: 400},
		},
		Bins: []inventory.LoopBin{
			{ID: 1, Label: "B1", Node: "ALN_009", PayloadCode: "P-DOUBT", UOP: -3, Confirmed: true},
			{ID: 2, Label: "B2", Node: "ALN_010", PayloadCode: "P-LDOUBT", UOP: 60, Confirmed: false},
			{ID: 3, Label: "B3", Node: "ALN_011", PayloadCode: "P-LOK", UOP: 60, Confirmed: true},
		},
		Claims: []inventory.LoopClaim{
			{ProcessID: "SNF2", CoreNodeName: "ALN_010", PayloadCode: "P-LDOUBT", Role: loaders.RoleConsume, Pack: 132, ReorderPoint: 20},
			{ProcessID: "SNF3", CoreNodeName: "ALN_011", PayloadCode: "P-LOK", Role: loaders.RoleConsume, Pack: 132, ReorderPoint: 20},
		},
	})
	for node, want := range map[string]bool{
		"SMN_001": true, "SMN_002": false, "PLK_W1": true, "SMN_003": true, "SMN_004": true,
		"ALN_010": true, "ALN_011": false,
	} {
		if got := rowFor(t, v, node).Look; got != want {
			t.Errorf("%s: look %v, want %v", node, got, want)
		}
	}
	if r := rowFor(t, v, "SMN_001"); !r.Beyond {
		t.Error("SMN_001 at its threshold is not beyond it")
	}

	// A home that refills when it empties (one bin, a threshold of a pack or
	// more: SPR's pack+1 thresholds) shows when its home is empty, not when
	// in-loop sits at or under its threshold, which it does whenever its one
	// bin is all the part has (orc, 2026-10-30: 15 of SPR's 22 opened by
	// default under the plain rule).
	refill := BuildLoopView(LoopInputsRead{
		Bindings: []inventory.Binding{
			binding("SMN_018", "P-HOME", loaders.LayoutDedicatedPositions, 251, 250, 1),   // its bin at home
			binding("SMN_033", "P-AWAY", loaders.LayoutDedicatedPositions, 251, 250, 1),   // its bin at the line
			binding("SMN_026", "P-ZERO", loaders.LayoutDedicatedPositions, 421, 420, 1),   // a bin at home at 0
			binding("SMN_035", "P-NONE", loaders.LayoutDedicatedPositions, 4500, 4500, 1), // no bin anywhere
		},
		Sums: []PayloadSystemUOP{
			{PayloadCode: "P-HOME", TotalUOP: 250}, {PayloadCode: "P-AWAY", TotalUOP: 250},
			{PayloadCode: "P-ZERO", TotalUOP: 0}, {PayloadCode: "P-NONE", TotalUOP: 0},
		},
		Bins: []inventory.LoopBin{
			{ID: 5, Label: "B5", Node: "SMN_018", PayloadCode: "P-HOME", UOP: 250, Confirmed: true},
			{ID: 6, Label: "B6", Node: "ALN_011", PayloadCode: "P-AWAY", UOP: 250, Confirmed: true},
			{ID: 7, Label: "B7", Node: "SMN_026", PayloadCode: "P-ZERO", UOP: 0, Confirmed: true},
		},
	})
	for node, want := range map[string]bool{"SMN_018": false, "SMN_033": true, "SMN_026": true, "SMN_035": true} {
		r := rowFor(t, refill, node)
		if r.State != LoopRefillsWhenEmpty || r.Beyond != want || r.Look != want {
			t.Errorf("%s (refills when empty): state %q, beyond %v, look %v; want beyond and look %v", node, r.State, r.Beyond, r.Look, want)
		}
	}

	healthy := BuildLoopView(LoopInputsRead{
		Bindings: []inventory.Binding{
			binding("SMN_010", "P-A", loaders.LayoutDedicatedPositions, 100, 500, 1),
			binding("SMN_011", "P-B", loaders.LayoutDedicatedPositions, 50, 500, 1),
		},
		Sums: []PayloadSystemUOP{{PayloadCode: "P-A", TotalUOP: 101}, {PayloadCode: "P-B", TotalUOP: 499}},
		Bins: []inventory.LoopBin{{ID: 4, Label: "B4", Node: "ALN_020", PayloadCode: "P-C", UOP: 90, Confirmed: true}},
		Claims: []inventory.LoopClaim{
			{ProcessID: "SNF4", CoreNodeName: "ALN_020", PayloadCode: "P-C", Role: loaders.RoleConsume, Pack: 132, ReorderPoint: 20},
		},
	})
	for _, r := range healthy.Rows {
		if r.Look || r.Beyond {
			t.Errorf("nothing qualifies, yet %s needs a look (beyond %v)", r.Node, r.Beyond)
		}
	}
	if len(healthy.Rows) != 3 {
		t.Errorf("rows = %d, want 3: a healthy row folds, it is not dropped", len(healthy.Rows))
	}
}

// TestBuildLoopView_SuggestionNeedsACompletedL1 pins when a row carries the
// calculator's suggestion: only once the position has a completed threshold L1
// cycle in the window (I9). At 03198443 it carried one whenever the part had a
// measured rate and a lead time — SPR's SMN_020: one order queued and
// dispatched, never completed, a 0.023 s lead and a suggestion of 1.
func TestBuildLoopView_SuggestionNeedsACompletedL1(t *testing.T) {
	t.Parallel()
	none := binding("SMN_020", "P-NOCYCLE", loaders.LayoutDedicatedPositions, 100, 500, 1)
	none.CompletedL1 = 0
	one := binding("SMN_021", "P-NOCYCLE", loaders.LayoutDedicatedPositions, 100, 500, 1)
	v := BuildLoopView(LoopInputsRead{
		Bindings: []inventory.Binding{none, one},
		Sums:     []PayloadSystemUOP{{PayloadCode: "P-NOCYCLE", TotalUOP: 50}},
		Leads: map[string]orders.LeadTimeInputs{
			"P-NOCYCLE": {L1QueueSeconds: 0.023, SamplesL1: 0, RatePerSec: 0.1},
		},
	})
	if r := rowFor(t, v, "SMN_020"); r.Suggested != nil || r.Confidence != "" || r.CompletedL1 != 0 {
		t.Errorf("0 completed L1 cycles: suggestion %v %q; want none", r.Suggested, r.Confidence)
	}
	r := rowFor(t, v, "SMN_021")
	if r.Suggested == nil || r.CompletedL1 != 1 {
		t.Fatalf("1 completed L1 cycle at the position: no suggestion")
	}
	t.Logf("PIN inventory-2/suggestion-one-l1 suggested=%d", *r.Suggested)
}
