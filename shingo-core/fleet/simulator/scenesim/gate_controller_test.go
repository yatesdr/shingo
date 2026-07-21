package scenesim

import (
	"sort"
	"testing"
)

// gateController is the harness-side stand-in for Core's lane-gate release
// evaluator (dispatch/lane_gate_release.go EvaluateLaneReleases).
//
// WHAT IT IS AND IS NOT. Core's evaluator is asserted directly, against a real
// database and a recording fleet backend, in the dispatch docker battery — that
// is where "does the production code implement this policy" is answered. What the
// physics harness answers is the other half: "does this policy, executed over real
// single-file lane physics, actually keep the lane wall-free and deadlock-free."
// Those are different questions and only the second one needs a scene.
//
// The policy below is therefore a deliberate restatement of classifyLaneEntry
// (dispatch/lane_entry.go), kept structurally parallel to it so a reader can diff
// the two by eye:
//
//	same origin      → never gate each other        (Tier 1, co-release)
//	deeper pending   → hold                         (Tier 2, deepest-first)
//	active group     → hold                         (Tier 3, group wait)
//
// and the blocker set is the A′ predicate — an order blocks until it PLACES, not
// until it completes — which in the sim is exactly "its bin is not yet in its
// slot".
type gateController struct {
	sim  *Sim
	lane string

	staged []*gateOrder
	byID   map[string]*gateOrder

	// firesPerTick runs the evaluator more than once per tick, to prove repeated
	// firing changes nothing (the double-fire scenario). 0 means once.
	firesPerTick int

	appends    map[string]int  // order id → how many times its tail was appended
	releasedAt map[string]int  // order id → tick the gate released it (absent = never gated)
	placedAt   map[string]int  // order id → tick its bin landed (PLACEMENT)
	doneAt     map[string]int  // order id → tick its robot went idle (COMPLETION)
	placedSeq  []string        // order ids in placement order
	seen       map[string]bool // placement de-dup
	seenDone   map[string]bool // completion de-dup
}

type gateOrder struct {
	id     string
	robot  string
	slot   string
	origin string // "" = unclassified, which production treats as its OWN origin
	depth  int
	sealed bool // its tail has been appended
}

func newGateController(sim *Sim, lane string) *gateController {
	return &gateController{
		sim:        sim,
		lane:       lane,
		byID:       map[string]*gateOrder{},
		appends:    map[string]int{},
		releasedAt: map[string]int{},
		placedAt:   map[string]int{},
		doneAt:     map[string]int{},
		seen:       map[string]bool{},
		seenDone:   map[string]bool{},
	}
}

// submit dispatches one store through the valve: add the robot, create the
// UNSEALED waybill ending at the gate, then immediately evaluate — so an
// uncontended order has its tail appended back to back with the create (the open
// valve) and a contended one is left dwelling for a later firing.
func (g *gateController) submit(t *testing.T, robot, orderID, slot, origin string, approach int) {
	t.Helper()
	depth, ok := g.sim.scene.SlotDepth(slot)
	if !ok {
		t.Fatalf("slot %q is not in lane %s", slot, g.lane)
	}
	if err := g.sim.AddRobot(robot, "AISLE"); err != nil {
		t.Fatalf("AddRobot %s: %v", robot, err)
	}
	g.sim.SetRobotApproach(robot, approach)
	if err := g.sim.Submit(robot, gatedStoreReq(orderID, "LINE", "GATE"), false); err != nil {
		t.Fatalf("Submit %s: %v", orderID, err)
	}
	o := &gateOrder{id: orderID, robot: robot, slot: slot, origin: origin, depth: depth}
	g.staged = append(g.staged, o)
	g.byID[orderID] = o
	g.evaluate(t)
}

// evaluate is one release pass, mirroring EvaluateLaneReleases: take the staged
// set, order it DEEPEST FIRST, and append the tail of every order the classifier
// admits. Deepest-first is what makes Tier 2 do the rest — once the deep order is
// released it is still an un-placed blocker, so a shallower cross-origin order
// evaluated after it holds, while a same-origin partner is skipped by Tier 1 and
// goes out in this same pass.
func (g *gateController) evaluate(t *testing.T) {
	t.Helper()
	pending := make([]*gateOrder, 0, len(g.staged))
	for _, o := range g.staged {
		if !o.sealed {
			pending = append(pending, o)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].depth > pending[j].depth })

	for _, o := range pending {
		if g.holds(o) {
			continue
		}
		if err := g.sim.AppendBlocks(o.id, gatedTail(o.id, o.slot)); err != nil {
			t.Fatalf("AppendBlocks %s: %v", o.id, err)
		}
		o.sealed = true
		g.appends[o.id]++
		// Tick 0 is the pre-run submit — an order sealed there went through an OPEN
		// valve and was never really gated, so it gets no release tick.
		if tick := g.sim.TickCount(); tick > 0 {
			g.releasedAt[o.id] = tick
		}
	}
}

// holds applies the classifier to one staged order against live occupancy.
func (g *gateController) holds(self *gateOrder) bool {
	// Blockers: orders that are working this lane and have NOT yet placed. The A′
	// predicate — a store stops blocking when its bin is DOWN, not when its whole
	// order finishes.
	originCount := map[string]int{}
	var others []*gateOrder
	for _, o := range g.staged {
		if o.id == self.id || g.sim.HasBin(o.slot) {
			continue
		}
		others = append(others, o)
		if o.origin != "" {
			originCount[o.origin]++
		}
	}
	for _, o := range others {
		if self.origin != "" && self.origin == o.origin {
			continue // Tier 1 — same-origin partner, never gate
		}
		if o.depth > self.depth {
			return true // Tier 2 — a deeper cross-origin store has not placed yet
		}
		if o.origin != "" && originCount[o.origin] >= 2 {
			return true // Tier 3 — an active cross-origin group holds the lane
		}
	}
	return false
}

// run ticks the sim, firing the evaluator each tick (the event-driven trigger,
// modelled as a poll — the real one fires on placement and transit events, which
// is a subset of every tick, so polling can only be MORE forgiving about timing,
// never less). Returns the settle tick, all violations, and whether it settled.
func (g *gateController) run(t *testing.T, maxTicks int, each func()) (int, []Violation, bool) {
	t.Helper()
	var vios []Violation
	fires := g.firesPerTick
	if fires <= 0 {
		fires = 1
	}
	for range maxTicks {
		vios = append(vios, g.sim.Tick()...)
		g.recordPlacements()
		for range fires {
			g.evaluate(t)
		}
		if each != nil {
			each()
		}
		if g.sim.AllIdle() {
			return g.sim.TickCount(), vios, true
		}
	}
	return g.sim.TickCount(), vios, false
}

// recordPlacements stamps the tick each order's bin landed (placement) and the
// tick its robot went idle (completion), in order. Keeping both is what lets a
// scenario assert that a release happened on the FORMER while the blocker was
// still working — the difference between placement-release and the coarse
// completion-release the fallback arm has.
func (g *gateController) recordPlacements() {
	for _, o := range g.staged {
		if !g.seen[o.id] && g.sim.HasBin(o.slot) {
			g.seen[o.id] = true
			g.placedAt[o.id] = g.sim.TickCount()
			g.placedSeq = append(g.placedSeq, o.id)
		}
		if !g.seenDone[o.id] && g.seen[o.id] && !g.sim.OrderActive(o.id) {
			g.seenDone[o.id] = true
			g.doneAt[o.id] = g.sim.TickCount()
		}
	}
}

// placementOrder returns the order ids in the sequence their bins landed — the
// physical entry order the gate is supposed to control.
func (g *gateController) placementOrder() []string { return g.placedSeq }
