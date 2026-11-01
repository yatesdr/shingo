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
	for name, want := range map[string]int{"PRESS-KANBAN": 7, "WELD-KANBAN": 2, "ZONE-45": 6, "ZONE-32": 6} {
		q := plan.pools[name]
		if q == nil {
			t.Fatalf("no pool %s", name)
		}
		if !q.isMaintained || q.levelWant != want {
			t.Errorf("%s: maintained %v, want %d; expected %d", name, q.isMaintained, q.levelWant, want)
		}
		if q.seededEmpty < q.levelWant {
			t.Errorf("%s seeded %d against a want of %d", name, q.seededEmpty, q.levelWant)
		}
		if q.keeperIn != 0 || q.keeperOut != 0 {
			t.Errorf("%s keeper in %.2f out %.2f; a balanced loop moves nothing across a boundary", name, q.keeperIn, q.keeperOut)
		}
	}
	for _, name := range []string{"PRESS-KANBAN", "WELD-KANBAN"} {
		q := plan.pools[name]
		if q.emptyDemand <= 0 || q.emptyDemand != q.emptySupply {
			t.Errorf("%s spends %.3f and frees %.3f, want equal and above 0", name, q.emptyDemand, q.emptySupply)
		}
	}
	if !reportKanbans(p, plan, rate, 200) {
		t.Error("the rig does not pass the kanban check")
	}
}

// A kanban's shortfall settles against its first source, not its overflow.
func TestComputeCarriers_AShortfallSettlesAgainstTheFirstSource(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	// Cut the weld's unloader off, so K2 spends and nothing returns to it.
	kept := p.Claims[:0:0]
	for _, c := range p.Claims {
		if c.WindowOf != "KW-UNLOADER" {
			kept = append(kept, c)
		}
	}
	p.Claims = kept
	plan := computeCarriers(p, rate, 7.5, 7.5)
	in, out := plan.pools["WELD-KANBAN"].keeperIn, plan.pools["ZONE-45"].keeperOut
	if in <= 0 || in != out {
		t.Errorf("K2 keeper in %.3f, ZONE-45 keeper out %.3f, want the same shortfall on both", in, out)
	}
}

// THE CLOCK: what starves first with its returns stopped, the arithmetic the file's
// head and plants/README.md quote.
func TestKanbanPress_TheClock(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	clock := newKanbanView(p).clock(rate)
	if len(clock) != 2 {
		t.Fatalf("clock = %+v, want the two running types", clock)
	}
	first, second := clock[0], clock[1]
	if first.kanban != "WELD-KANBAN" || first.binType != "SMALL-45" || first.stock != 4 || int(first.mins+0.5) != 30 {
		t.Errorf("first to starve = %+v, want WELD-KANBAN's small, stock 4, 30 sim-min", first)
	}
	if second.kanban != "PRESS-KANBAN" || second.binType != "SMALL-45" || second.stock != 7 || int(second.mins+0.5) != 35 {
		t.Errorf("second = %+v, want PRESS-KANBAN's small, stock 7, 35 sim-min", second)
	}
}

// CHANGEOVER ROOM, the arithmetic behind R2 and R3: a changeover on the floor
// conserves; leaving it leaves 8 carts past the levels, and the plain overflow is
// the room for them. A cart with nowhere to land parks on a window, so no excess is
// what keeps KP-32's fulls a window. 32x30 is 2 short of K's want against 6 carts:
// cover over the pool, a statement about the plant and not a defect.
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
		if r := got[key{style, "45x48"}]; r.carts != 17 || r.levels != 17 || r.toSpare != 0 || r.excess != 0 || r.short != 0 {
			t.Errorf("%s on 45x48 = %+v, want 17 carts against 17 of level and want", style, r)
		}
	}
	r := got[key{"KP-32", "45x48"}]
	if r.carts != 17 || r.levels != 9 || r.spare != 10 || r.toSpare != 8 || !slices.Equal(r.spareZone, []string{"ZONE-45-SPARE"}) {
		t.Errorf("KP-32 on 45x48 = %+v, want 9 on K and Z45's levels and 8 in the spare zone's 10 slots", r)
	}
	if r.excess != 0 {
		t.Errorf("KP-32 on 45x48 parks %d carts on the windows, and KP-32's fulls lose them", r.excess)
	}
	// The level of 32x30's own overflow is not room: it refuses at level.
	if r := got[key{"KP-32", "30x32"}]; r.carts != 6 || r.lineside != 8 || r.spare != 0 || r.short != 2 || r.excess != 0 {
		t.Errorf("KP-32 on 30x32 = %+v, want the kanban 2 short and nothing parked", r)
	}
	if _, ok := got[key{"KP-UNTYPED", "45x48"}]; ok {
		t.Error("the style with no carrier changes no want and has no row")
	}

	// The weld's style does not leave the family, so K2 needs no overflow.
	k2, ok := v.groupNamed("WELD-KANBAN")
	if !ok {
		t.Fatal("no WELD-KANBAN")
	}
	if k2.Overflow != "" {
		t.Errorf("K2 overflows to %q; its one style stays on the floor", k2.Overflow)
	}
	for _, r := range v.changeoverRoom(k2) {
		if r.toSpare != 0 || r.excess != 0 || r.short != 0 {
			t.Errorf("K2 %+v, want it to fit with no overflow", r)
		}
	}

	// Red: without the plain overflow the same carts park.
	noSpare := g
	noSpare.Overflow = "ZONE-45"
	for _, r := range v.changeoverRoom(noSpare) {
		if r.style == "KP-32" && r.floor == "45x48" && r.excess != 8 {
			t.Errorf("KP-32 overflowing to a levelled zone = %+v, want 8 parked", r)
		}
	}
}

// Red: a kanban seeded under its want, or a pool draining inside the run, fails the
// command.
func TestReportKanbans_FailsBelowWantAndOnADrainInsideTheRun(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	short := *p
	short.Bins = nil
	for _, b := range p.Bins {
		if b.Name != "KB-K-S5" {
			short.Bins = append(short.Bins, b)
		}
	}
	if reportKanbans(&short, computeCarriers(&short, rate, 7.5, 7.5), rate, 200) {
		t.Error("K seeded one under its want passed")
	}

	plan := computeCarriers(p, rate, 7.5, 7.5)
	plan.pools["WELD-KANBAN"].emptySupply = 0
	if reportKanbans(p, plan, rate, 200) {
		t.Error("K2 with nothing returning passed a 200 sim-minute run")
	}
	if !reportKanbans(p, plan, rate, 10) {
		t.Error("a 10 sim-minute run is inside K2's stock and should pass")
	}
}
