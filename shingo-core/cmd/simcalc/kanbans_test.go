package main

import (
	"path/filepath"
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
	for name, want := range map[string]int{"PRESS-KANBAN": 8, "WELD-KANBAN": 2} {
		q := plan.pools[name]
		if q == nil {
			t.Fatalf("no pool %s", name)
		}
		if !q.isKanban || q.levelWant != want {
			t.Errorf("%s: kanban %v, want %d; expected %d", name, q.isKanban, q.levelWant, want)
		}
		if q.seededEmpty < q.levelWant {
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

// K's cover is the 4 carts on the positions, and they count: K's small want of 6 is its 2 and
// the line's 4, and the plant's own file passes.
func TestKanbanPress_KHoldsItsWantOfTheRunningType(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	v := newKanbanView(p)
	g, ok := v.groupNamed("PRESS-KANBAN")
	if !ok {
		t.Fatal("no PRESS-KANBAN")
	}
	want := map[string][3]int{"SMALL-45": {6, 2, 4}, "BIG-45": {2, 2, 0}, "TOTES-45": {2, 2, 0}, "CART-32": {2, 6, 0}}
	for _, l := range v.lines(g, v.activeStyles()) {
		if w := want[l.binType]; l.want != w[0] || l.seeded != w[1] || l.held != w[2] {
			t.Errorf("K's %s = want %d, seeded %d, held %d; expected %v", l.binType, l.want, l.seeded, l.held, w)
		}
	}
	if !reportKanbans(p, computeCarriers(p, rate, 7.5, 7.5), rate, 200) {
		t.Error("the plant's own file did not pass the kanban check")
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

// THE FAMILIES, the arithmetic behind R2, R3 and the walk: 45x48 is 10 carts on a minimum of
// 6 + 4 and 10 slots, 32x30 is 6 on 2 + 4 and 6, and the weld's small is its own 3.
func TestKanbanPress_TheFamilies(t *testing.T) {
	t.Parallel()
	p, _ := kanbanPress(t)
	v := newKanbanView(p)
	want := map[string]map[string]familyLine{
		"PRESS-KANBAN": {
			"45x48":   {family: "45x48", pool: 10, minimum: 10, room: 10, carts: 10},
			"CART-32": {family: "CART-32", pool: 6, minimum: 6, room: 6, carts: 6},
		},
		"WELD-KANBAN": {"45x48": {family: "45x48", pool: 3, minimum: 3, room: 4, carts: 3}},
	}
	for group, families := range want {
		g, ok := v.groupNamed(group)
		if !ok {
			t.Fatalf("no %s", group)
		}
		got := v.families(g)
		if len(got) != len(families) {
			t.Errorf("%s has %d families, want %d: %+v", group, len(got), len(families), got)
		}
		for _, f := range got {
			if f != families[f.family] {
				t.Errorf("%s %s = %+v, want %+v", group, f.family, f, families[f.family])
			}
		}
	}
}

// Red: a kanban seeded under its want, or a pool draining inside the run, fails the
// command. K made whole by hand first, because the fixture seats it short.
func TestReportKanbans_FailsBelowWantAndOnADrainInsideTheRun(t *testing.T) {
	t.Parallel()
	p, rate := kanbanPress(t)
	whole := *p
	if !reportKanbans(&whole, computeCarriers(&whole, rate, 7.5, 7.5), rate, 200) {
		t.Error("the plant's own file did not pass")
	}
	short := whole
	short.Bins = nil
	for _, b := range whole.Bins {
		if b.Name != "KB-K-S2" {
			short.Bins = append(short.Bins, b)
		}
	}
	if reportKanbans(&short, computeCarriers(&short, rate, 7.5, 7.5), rate, 200) {
		t.Error("K one under its want and its pool passed")
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
