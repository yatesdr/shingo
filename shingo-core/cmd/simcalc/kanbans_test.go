package main

import (
	"path/filepath"
	"slices"
	"testing"

	"shingocore/plantspec"
)

// kanbans_test.go — the rig's numbers as simcalc computes them, off the shipped
// plant and dev config: this is the check `simcalc -carriers` prints, as a test.

func kanbanPress(t *testing.T) (*plantspec.Plant, map[string]float64) {
	t.Helper()
	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "kanban-press.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	edge, err := loadEdge(filepath.Join("..", "..", "..", "shingo-edge", "shingoedge.dev.yaml"))
	if err != nil {
		t.Fatalf("load edge: %v", err)
	}
	return p, edgeRates(edge)
}

// A loader's allowed list is its parts: each flowing part is drained, one no style
// runs makes no flow of its own, so the unloader reads DRAIN and not NO-SOURCE.
func TestWalkClaims_AnUnloadersAllowedListDrainsWhatFlows(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	flows := walkClaims(p, rate)
	for _, part := range []string{"KP-SM-L", "KP-SM-R", "KW-PART"} {
		f := flows[part]
		if f == nil {
			t.Fatalf("%s has no flow", part)
		}
		if label, healthy, detail := f.verdict(7.5, 7.5); !healthy || label != "DRAIN" {
			t.Errorf("%s = %s (%s), want a healthy DRAIN", part, label, detail)
		}
	}
	for _, idle := range []string{"KP-BIG", "KP-TOTE", "KP-32", "KP-UNTYPED", ""} {
		if f := flows[idle]; f != nil {
			t.Errorf("part %q, which no running style makes, has a flow: %+v", idle, f)
		}
	}
}

// Sized for the run: both pools balance at the configured ticks, the level the pool
// is judged on is the want and not the bare level, and nothing crosses a boundary in
// steady state.
func TestKanbanPress_PoolsBalanceAndAreJudgedOnTheirWant(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	plan := computeCarriers(p, rate, 7.5, 7.5)
	for name, want := range map[string]int{"PRESS-KANBAN": 12, "WELD-KANBAN": 2} {
		q := plan.pools[name]
		if q == nil {
			t.Fatalf("no pool %s", name)
		}
		if !q.isKanban || q.levelWant != want {
			t.Errorf("%s: kanban %v, want %d; expected %d", name, q.isKanban, q.levelWant, want)
		}
		if q.seededEmpty != q.levelWant {
			t.Errorf("%s seeded %d against a want of %d", name, q.seededEmpty, q.levelWant)
		}
		if q.keeperIn != 0 || q.keeperOut != 0 {
			t.Errorf("%s keeper in %.2f out %.2f; a balanced loop moves nothing across a boundary", name, q.keeperIn, q.keeperOut)
		}
		if q.emptyDemand <= 0 || q.emptyDemand != q.emptySupply {
			t.Errorf("%s spends %.3f and frees %.3f, want equal and above 0", name, q.emptyDemand, q.emptySupply)
		}
	}
}

// K's cover is the 4 carts on the positions, so K seats 4 under its want of the running
// type: the fixture's numbers, and simcalc says so and fails. The pool total above hides it,
// 12 of want against 12 seeded, because K's 32x30 stand 4 over.
func TestKanbanPress_KSeatsBelowItsWantOfTheRunningType(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	v := newKanbanView(p)
	g, ok := v.groupNamed("PRESS-KANBAN")
	if !ok {
		t.Fatal("no PRESS-KANBAN")
	}
	want := map[string][2]int{"SMALL-45": {6, 2}, "BIG-45": {2, 2}, "TOTES-45": {2, 2}, "CART-32": {2, 6}}
	for _, l := range v.lines(g, v.activeStyles()) {
		if w := want[l.binType]; l.want != w[0] || l.seeded != w[1] {
			t.Errorf("K's %s = want %d, seeded %d; expected %v", l.binType, l.want, l.seeded, w)
		}
	}
	if reportKanbans(p, computeCarriers(p, rate, 7.5, 7.5), rate, 200) {
		t.Error("K seats 4 under its want of small, and the kanban check passed")
	}
}

// A kanban's shortfall settles against its first source, not its overflow. The rig's
// kanbans have no source, so this adds one.
func TestComputeCarriers_AShortfallSettlesAgainstTheFirstSource(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	p.Zones = append(p.Zones, plantspec.Zone{Name: "LENDER", Positions: []plantspec.Slot{{Name: "LND_001", Depth: 1}}})
	p.Kanbans = append(p.Kanbans, plantspec.Kanban{Group: "LENDER", Levels: []plantspec.KanbanLevel{{BinType: "SMALL-45", Want: 1}}})
	for i := range p.Kanbans {
		if p.Kanbans[i].Group == "WELD-KANBAN" {
			p.Kanbans[i].Sources = []string{"LENDER"}
		}
	}
	// Cut the weld's unloader off, so K2 spends and nothing returns to it.
	kept := p.Claims[:0:0]
	for _, c := range p.Claims {
		if c.WindowOf != "KW-UNLOADER" {
			kept = append(kept, c)
		}
	}
	p.Claims = kept
	plan := computeCarriers(p, rate, 7.5, 7.5)
	in, out := plan.pools["WELD-KANBAN"].keeperIn, plan.pools["LENDER"].keeperOut
	if in <= 0 || in != out {
		t.Errorf("K2 keeper in %.3f, LENDER keeper out %.3f, want the same shortfall on both", in, out)
	}
}

// THE CLOCK: what starves first with its returns stopped, the arithmetic the file's
// head and plants/README.md quote. No lender behind either: K's 2 small, K2's 2.
func TestKanbanPress_TheClock(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	clock := newKanbanView(p).clock(rate)
	if len(clock) != 2 {
		t.Fatalf("clock = %+v, want the two running types", clock)
	}
	first, second := clock[0], clock[1]
	if first.kanban != "PRESS-KANBAN" || first.binType != "SMALL-45" || first.stock != 2 || int(first.mins+0.5) != 10 {
		t.Errorf("first to starve = %+v, want PRESS-KANBAN's small, stock 2, 10 sim-min", first)
	}
	if second.kanban != "WELD-KANBAN" || second.binType != "SMALL-45" || second.stock != 2 || int(second.mins+0.5) != 15 {
		t.Errorf("second = %+v, want WELD-KANBAN's small, stock 2, 15 sim-min", second)
	}
}

// CHANGEOVER ROOM, the arithmetic behind R2, R3 and the walk. A style on 45x48 leaves its
// floor's 10 carts against K's 10 of want and the 4 positions: K 4 short, and all of it
// cover over the pool. 32x30 has 4 past K's level of 2 with nowhere to land if they leave K;
// as seeded they stand in it. KP-32 is the mirror: 4 45x48 past K's levels, no room for them,
// and those 4 are all 4 windows, so KP-32's fulls have none.
func TestKanbanPress_ChangeoverRoom(t *testing.T) {
	t.Parallel()
	p, _ := kanbanPress(t)
	v := newKanbanView(p)
	g, ok := v.groupNamed("PRESS-KANBAN")
	if !ok {
		t.Fatal("no PRESS-KANBAN")
	}
	type key struct{ style, floor string }
	got := map[key]roomLine{}
	for _, r := range v.changeoverRoom(g) {
		got[key{r.style, r.floor}] = r
	}
	for _, style := range []string{"KP-SMALL", "KP-BIG", "KP-TOTES"} {
		if r := got[key{style, "45x48"}]; r.carts != 10 || r.lineside != 14 || r.short != 4 || r.excess != 0 {
			t.Errorf("%s on 45x48 = %+v, want 10 carts, 14 of want and positions, 4 short", style, r)
		}
		r := got[key{style, "30x32"}]
		if r.carts != 6 || r.lineside != 2 || r.excess != 4 || r.short != 0 {
			t.Errorf("%s on 30x32 = %+v, want 6 carts, K's level of 2, 4 past it", style, r)
		}
		wantSeated := 0
		if style == "KP-SMALL" {
			wantSeated = 4
		}
		if r.seated != wantSeated {
			t.Errorf("%s on 30x32 seats %d above their want, want %d", style, r.seated, wantSeated)
		}
	}
	r := got[key{"KP-32", "45x48"}]
	if r.carts != 10 || r.lineside != 6 || r.spare != 0 || r.toSpare != 0 || r.excess != 4 || r.windows != 4 {
		t.Errorf("KP-32 on 45x48 = %+v, want 4 of 10 carts past K's 6, no room, and the 4 windows", r)
	}
	if r := got[key{"KP-32", "30x32"}]; r.carts != 6 || r.lineside != 10 || r.short != 4 || r.excess != 0 {
		t.Errorf("KP-32 on 30x32 = %+v, want K's 6 and the 4 positions from 6 carts", r)
	}
	if _, ok := got[key{"KP-UNTYPED", "45x48"}]; ok {
		t.Error("the style with no carrier changes no want and has no row")
	}

	// The weld's style does not leave the family, so K2 needs no overflow.
	k2, ok := v.groupNamed("WELD-KANBAN")
	if !ok {
		t.Fatal("no WELD-KANBAN")
	}
	if k2.Overflow != "" || len(k2.Sources) != 0 {
		t.Errorf("K2 overflows to %q and sources %v; it has its own carts and nothing else", k2.Overflow, k2.Sources)
	}
	for _, r := range v.changeoverRoom(k2) {
		if r.toSpare != 0 || r.excess != 0 || r.short != 0 {
			t.Errorf("K2 %+v, want it to fit with no overflow", r)
		}
	}

	// Red: the same carts with a plain overflow land in its room and take no window.
	withRoom := g
	withRoom.Overflow = "KP-SM"
	for _, r := range v.changeoverRoom(withRoom) {
		if r.style == "KP-32" && r.floor == "45x48" && (r.toSpare != 4 || r.excess != 0 || !slices.Equal(r.spareZone, []string{"KP-SM"})) {
			t.Errorf("KP-32 overflowing to a plain zone = %+v, want the 4 in its room", r)
		}
	}
}

// Red: a kanban seeded under its want, or a pool draining inside the run, fails the
// command. K made whole by hand first, because the fixture seats it short.
func TestReportKanbans_FailsBelowWantAndOnADrainInsideTheRun(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	whole := *p
	whole.Bins = append([]plantspec.Bin(nil), p.Bins...)
	for _, slot := range []string{"PKB_007", "PKB_008", "PKB_009", "PKB_010"} {
		whole.Bins = append(whole.Bins, plantspec.Bin{Name: "HAND-" + slot, Slot: slot, BinType: "SMALL-45"})
	}
	if !reportKanbans(&whole, computeCarriers(&whole, rate, 7.5, 7.5), rate, 200) {
		t.Error("K whole by hand did not pass")
	}
	short := whole
	short.Bins = nil
	for _, b := range whole.Bins {
		if b.Name != "HAND-PKB_010" {
			short.Bins = append(short.Bins, b)
		}
	}
	if reportKanbans(&short, computeCarriers(&short, rate, 7.5, 7.5), rate, 200) {
		t.Error("K one under its want passed")
	}

	plan := computeCarriers(&whole, rate, 7.5, 7.5)
	plan.pools["WELD-KANBAN"].emptySupply = 0
	if reportKanbans(&whole, plan, rate, 200) {
		t.Error("K2 with nothing returning passed a 200 sim-minute run")
	}
	if !reportKanbans(&whole, plan, rate, 10) {
		t.Error("a 10 sim-minute run is inside K2's stock and should pass")
	}
}
