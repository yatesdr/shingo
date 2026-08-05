package scenesim

import (
	"fmt"
	"math/rand"
	"testing"

	"shingocore/fleet"
)

// The dig-GROUP matrix: several robots working one reshuffle.
//
// A dig used to be one multi-leg order bound to one robot, so "one robot inside
// the lane" and "one dig working the lane" were the same statement and neither
// could be tested apart from the other. Groups separate them, and these cases
// are what the separation has to survive before production is allowed to
// dispatch reshuffle legs concurrently.
//
// EVERY CASE RECORDS THE MUTATION THAT PROVES IT NON-VACUOUS. Four checkers now
// run every tick and fire early and hard, so a green case may be green because a
// checker held rather than because the case asserted anything — that is not
// hypothetical, it is what happened when dig_test.go:127's first inversion was
// caught by the reachability checker before the assertion under test ran. Each
// doc comment below names the mutation used and what failed under it.

// digLegReq builds ONE leg of a group dig: a pickup and a dropoff.
//
// Deliberately not a mutation of digReq. digReq builds the whole four-block
// single-order dig, and every group-of-one test depends on it exactly as it is;
// touching it would put those tests' meaning at risk to save a helper.
func digLegReq(id, from, to string) fleet.CreateOrderRequest {
	return fleet.CreateOrderRequest{
		OrderID: id,
		Blocks: []fleet.OrderBlock{
			{BlockID: id + "-p", Location: from, BinTask: "JackLoad"},
			{BlockID: id + "-d", Location: to, BinTask: "JackUnload"},
		},
		Complete: true,
	}
}

// groupArms runs a case under both gate arms. The pilot gates with
// deepest-first (priorityOnly=false); the dig tests default to priority-only. A
// hold that works on one arm only is not a hold.
func groupArms(t *testing.T, run func(t *testing.T, priorityOnly bool)) {
	t.Helper()
	for _, arm := range []struct {
		name         string
		priorityOnly bool
	}{
		{"priority-only", true},
		{"deepest-first", false},
	} {
		t.Run(arm.name, func(t *testing.T) { run(t, arm.priorityOnly) })
	}
}

// groupSim builds a gated sim over a lane of `slots` slots, on the given arm.
func groupSim(t *testing.T, slots int, priorityOnly bool) *Sim {
	t.Helper()
	sim := New(wideLaneScene(t, slots), Options{Watchdog: 200, HopTicks: 3})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(priorityOnly)
	return sim
}

// laneOccupancy counts robots physically inside the lane right now.
func laneOccupancy(sim *Sim) int { return len(sim.committedTo("LANE")) }

// runGroup ticks until idle or the budget runs out, reporting checker
// violations and tracking the deepest simultaneous lane occupancy seen. The
// occupancy figure is read from where robots ACTUALLY ARE (committedTo, the
// same derived position state the checker reads), never from a flag the test
// set for itself.
func runGroup(t *testing.T, sim *Sim, budget int, each func(tick int)) (maxInside int, settled bool) {
	t.Helper()
	for tick := 0; tick < budget; tick++ {
		for _, v := range sim.Tick() {
			t.Errorf("checker fired at tick %d: %s: %s", sim.TickCount(), v.Checker, v.Detail)
		}
		if n := laneOccupancy(sim); n > maxInside {
			maxInside = n
		}
		if each != nil {
			each(tick)
		}
		if sim.AllIdle() {
			return maxInside, true
		}
	}
	return maxInside, false
}

// ── A: two legs of one dig ──────────────────────────────────────────────────

// TestDigGroup_TwoLegsOneInsideAtATime is case A: two robots carrying two legs
// of ONE dig. Both legs run; never both in the lane.
//
// The lane holds a blocker at S0 and the target at S1. Leg one lifts the
// blocker, leg two takes the target — the same work the single-order dig does,
// split across two robots, which is the whole point of groups.
//
// MUTATION (verified): relax admitToLane's occupant loop so a same-group dig
// entrant is admitted while a sibling is inside. dig-occupancy then fires from
// tick 8 with "lane LANE is held by dig group \"g1\" but 2 robots are inside:
// [LEG-A=g1, LEG-B=g1]" — same group, both inside, which is the Hold B failure
// mode purity cannot see (one group, one mode, no impurity).
//
// A FIRST ATTEMPT AT THIS CASE WAS VACUOUS and is worth recording. It had leg
// one take the mouth slot and leg two a deeper one; single-file, leg two would
// have had to pass through leg one's cell, so two-inside was impossible whatever
// the gate said, and the mutation produced no failure at all. The geometry above
// is the fix.
func TestDigGroup_TwoLegsOneInsideAtATime(t *testing.T) {
	groupArms(t, func(t *testing.T, priorityOnly bool) {
		sim := groupSim(t, 3, priorityOnly)
		// GEOMETRY CHOSEN SO CO-OCCUPANCY IS PHYSICALLY POSSIBLE. An earlier
		// version had leg one take the MOUTH slot and leg two a deeper one, and
		// it was structurally vacuous: single-file, leg two would have to pass
		// through the cell leg one is standing in, so two-inside could never
		// happen whatever the gate rules said. Its mutation duly produced no
		// failure.
		//
		// Here leg one goes DEEP (picks S1) and leg two works the SHALLOW slot
		// behind it (drops into S0). That is the stacking shape the mouth gate
		// permits for same-KIND robots and forbids for digs — so if the dig rule
		// stopped holding, leg two would legally line up behind leg one and the
		// lane would have two robots in it.
		sim.PlaceBin("S1") // leg one's target, deep; S0 left clear so it is reachable
		for _, id := range []string{"LEG-A", "LEG-B"} {
			if err := sim.AddRobot(id, "AISLE"); err != nil {
				t.Fatalf("AddRobot %s: %v", id, err)
			}
		}

		if err := sim.SubmitDigLeg("LEG-A", digLegReq("g1-l1", "S1", "LINE"), "g1"); err != nil {
			t.Fatalf("submit leg 1: %v", err)
		}
		// Leg two is submitted while leg one is still working: both legs of the
		// group are in flight at once, which is exactly what production is about
		// to start doing.
		if err := sim.SubmitDigLeg("LEG-B", digLegReq("g1-l2", "LINE", "S0"), "g1"); err != nil {
			t.Fatalf("submit leg 2: %v", err)
		}
		sim.SealDigGroup("g1")

		maxInside, settled := runGroup(t, sim, 600, nil)
		if !settled {
			t.Fatalf("group did not settle")
		}
		if maxInside > 1 {
			t.Errorf("%d robots were inside the lane at once; a dig-held lane admits exactly one", maxInside)
		}
		if sim.HasBin("S1") {
			t.Error("leg one never took S1")
		}
		if !sim.HasBin("S0") {
			t.Error("leg two never placed into S0")
		}
	})
}

// ── B: three legs ───────────────────────────────────────────────────────────

// TestDigGroup_ThreeLegsNoStarvation is case B: three legs, three robots. Leg
// three does not enter while leg two is inside, and no leg is starved out.
//
// Starvation is the failure B exists for that A cannot show: with two legs a
// "one inside" rule is satisfied by running them in either order, but with three
// a hold that forgets a waiter leaves one leg parked forever. The settle
// assertion is what catches that, not the occupancy count.
//
// MUTATION (verified): drop leg three's submission. "S2 still holds a bin — its
// leg never completed" fires on both arms, while occupancy and settling stay
// green. That separation is the point: a case that asserted only the discipline
// would have passed a run in which a third of the work never happened.
func TestDigGroup_ThreeLegsNoStarvation(t *testing.T) {
	groupArms(t, func(t *testing.T, priorityOnly bool) {
		sim := groupSim(t, 4, priorityOnly)
		for i := range 3 {
			sim.PlaceBin(fmt.Sprintf("S%d", i))
		}
		for _, id := range []string{"LEG-A", "LEG-B", "LEG-C"} {
			if err := sim.AddRobot(id, "AISLE"); err != nil {
				t.Fatalf("AddRobot %s: %v", id, err)
			}
		}
		legs := []struct{ robot, id, slot string }{
			{"LEG-A", "g2-l1", "S0"},
			{"LEG-B", "g2-l2", "S1"},
			{"LEG-C", "g2-l3", "S2"},
		}
		for _, l := range legs {
			if err := sim.SubmitDigLeg(l.robot, digLegReq(l.id, l.slot, "LINE"), "g2"); err != nil {
				t.Fatalf("submit %s: %v", l.id, err)
			}
		}
		sim.SealDigGroup("g2")

		maxInside, settled := runGroup(t, sim, 900, nil)
		if !settled {
			t.Fatalf("three-leg group did not settle — a leg was starved at the mouth")
		}
		if maxInside > 1 {
			t.Errorf("%d robots were inside at once; a dig-held lane admits exactly one", maxInside)
		}
		for i := range 3 {
			if slot := fmt.Sprintf("S%d", i); sim.HasBin(slot) {
				t.Errorf("%s still holds a bin — its leg never completed", slot)
			}
		}
	})
}

// ── C: the group claim spans the inter-leg gap ──────────────────────────────

// TestDigGroup_ForeignStoreHeldForWholeGroup is case C, and it is the case that
// justifies giving the claim its own lifetime.
//
// A foreign store wants a slot the dig frees. It must be held out for the WHOLE
// GROUP — including the gap between leg one finishing and leg two starting. A
// claim derived from "some leg is active" evaporates in that gap and lets the
// store in, which is bit-for-bit the production bug where Hold A died at the
// first leg's completion. The legs here are submitted with a real gap between
// them: leg two goes in only after leg one has gone idle.
//
// MUTATION (verified): seal the group at leg one's submission, so the claim
// closes the moment leg one goes idle. This case's OWN assertion then fires on
// both arms — "the foreign store entered the lane while the dig GROUP was still
// working" — and no checker fires first, because the store is alone in the lane
// and nothing about that is impure or unreachable. The case carries its own
// weight.
//
// THE GAP HAD TO BE WIDENED TO GET THERE. The first version submitted leg two on
// the same tick leg one went idle, so the claim lapsed and was restored inside
// one tick and the store never had time to move — the mutation produced no
// failure and the case was vacuous.
func TestDigGroup_ForeignStoreHeldForWholeGroup(t *testing.T) {
	groupArms(t, func(t *testing.T, priorityOnly bool) {
		sim := groupSim(t, 3, priorityOnly)
		sim.PlaceBin("S0")
		sim.PlaceBin("S1")
		for _, id := range []string{"LEG-A", "LEG-B", "STORE"} {
			if err := sim.AddRobot(id, "AISLE"); err != nil {
				t.Fatalf("AddRobot %s: %v", id, err)
			}
		}
		if err := sim.SubmitDigLeg("LEG-A", digLegReq("g3-l1", "S0", "LINE"), "g3"); err != nil {
			t.Fatalf("submit leg 1: %v", err)
		}
		// The store wants S0 — the slot leg one is about to free — so it has a
		// real reason to race in the moment that leg finishes.
		if err := sim.Submit("STORE", storeReq("store-1", "LINE", "S0"), false); err != nil {
			t.Fatalf("submit store: %v", err)
		}

		var storeEnteredDuringGroup bool
		var legTwoSubmitted bool
		legOneDone := -1
		groupWorking := func() bool { return sim.OrderActive("g3-l1") || sim.OrderActive("g3-l2") || !legTwoSubmitted }

		_, settled := runGroup(t, sim, 900, func(tick int) {
			// THE GAP MUST BE WIDE ENOUGH TO DRIVE THROUGH. Submitting leg two on
			// the same tick leg one goes idle leaves no gap at all: the claim
			// lapses and is re-established within one tick, and the store never
			// gets a chance to move. That made an earlier version of this case
			// VACUOUS — its mutation produced no failure. gapTicks is sized past
			// the store's travel time (HopTicks=3) so a lapsed claim is
			// immediately visible as a store in the lane.
			const gapTicks = 30
			if legOneDone < 0 && !sim.OrderActive("g3-l1") {
				legOneDone = tick
			}
			if !legTwoSubmitted && legOneDone >= 0 && tick >= legOneDone+gapTicks {
				if err := sim.SubmitDigLeg("LEG-B", digLegReq("g3-l2", "S1", "LINE"), "g3"); err != nil {
					t.Fatalf("submit leg 2: %v", err)
				}
				sim.SealDigGroup("g3")
				legTwoSubmitted = true
			}
			if sr := sim.robots["STORE"]; sr != nil && sr.pos.inLane() && groupWorking() {
				storeEnteredDuringGroup = true
			}
		})
		if !settled {
			t.Fatalf("scenario did not settle")
		}
		if storeEnteredDuringGroup {
			t.Error("the foreign store entered the lane while the dig GROUP was still working — " +
				"the claim did not span the gap between legs, which is the failure this case exists for")
		}
		if !sim.HasBin("S0") {
			t.Error("the store never placed into S0 after the group finished — the claim may never have released")
		}
	})
}

// ── D: foreign retrieve, the outbound side ─────────────────────────────────

// TestDigGroup_GatedRetrieveDwellsForWholeGroup is case D: the outbound side of
// C. A retrieve wanting a bin the dig will expose must be held for the WHOLE
// group, including the gap between legs.
//
// IT USES THE GATE SHAPE, and that is precedent rather than convenience. An
// outbound order on a dig-held lane is buried by construction — the bin it wants
// is behind the ones the dig is lifting — so binding it to a lane slot up front
// makes checkReachability fire, correctly: it genuinely cannot reach that slot
// yet. Production solves this with gate choreography (the order ships bound to a
// wait point and Core appends its tail when the lane opens), and
// TestBuriedRetrieve_PrePositionsDuringDig already models exactly that for a
// group-of-one dig. This is that scenario against a GROUP.
//
// The tail is appended DURING THE INTER-LEG GAP, deliberately. From that moment
// the retrieve is able to enter and only the group's claim is stopping it — so
// if the claim evaporated between legs, the retrieve would go in and the dwell
// assertion would fail. That is what makes this a test of the claim and not of
// the test's own scheduling.
//
// MUTATION (proves non-vacuous): sealing the group at leg one's submission, so
// the claim closes when leg one goes idle. The retrieve then enters during the
// gap and "entered while the group was still working" fires. No checker catches
// it — the retrieve is alone in the lane — so this case carries its own weight.
func TestDigGroup_GatedRetrieveDwellsForWholeGroup(t *testing.T) {
	groupArms(t, func(t *testing.T, priorityOnly bool) {
		requireOutboundPhysics(t)
		sim := groupSim(t, 3, priorityOnly)
		sim.PlaceBin("S0") // leg one lifts this
		sim.PlaceBin("S1") // leg two lifts this; what the retrieve is waiting on
		for _, id := range []string{"LEG-A", "LEG-B", "RETR"} {
			if err := sim.AddRobot(id, "AISLE"); err != nil {
				t.Fatalf("AddRobot %s: %v", id, err)
			}
		}
		if err := sim.SubmitDigLeg("LEG-A", digLegReq("g4-l1", "S0", "LINE"), "g4"); err != nil {
			t.Fatalf("submit leg 1: %v", err)
		}
		// Ships now, bound only to the wait point — the pre-positioning increment.
		if err := sim.Submit("RETR", gatedRetrieveReq("retr-1", "GATE"), false); err != nil {
			t.Fatalf("submit gated retrieve: %v", err)
		}

		var (
			legTwoSubmitted bool
			tailAppended    bool
			dwelled         bool
			enteredDuring   bool
		)
		legOneDone := -1
		_, settled := runGroup(t, sim, 900, func(tickNow int) {
			groupWorking := !legTwoSubmitted || sim.OrderActive("g4-l1") || sim.OrderActive("g4-l2")
			rr := sim.robots["RETR"]
			if rr != nil && !rr.pos.inLane() && groupWorking {
				dwelled = true
			}
			if rr != nil && rr.pos.inLane() && groupWorking {
				enteredDuring = true
			}
			if legOneDone < 0 && !sim.OrderActive("g4-l1") {
				legOneDone = tickNow
			}
			// THE TAIL GOES ON AT THE START OF THE GAP, leg two only at its end.
			// Appending both at the same instant leaves no window in which the
			// retrieve is able to move and only the CLAIM is stopping it — which
			// made an earlier version of this case vacuous: its mutation produced
			// no failure at all, because the tail arrived exactly as the claim was
			// re-established. Now the retrieve is armed for ~30 ticks of gap.
			if !tailAppended && legOneDone >= 0 && tickNow >= legOneDone+1 {
				if err := sim.AppendBlocks("retr-1", []fleet.OrderBlock{
					{BlockID: "retr-1-p", Location: "S1", BinTask: "JackLoad"},
					{BlockID: "retr-1-d", Location: "LINE", BinTask: "JackUnload"},
				}); err != nil {
					t.Fatalf("append retrieve tail: %v", err)
				}
				tailAppended = true
			}
			if !legTwoSubmitted && legOneDone >= 0 && tickNow >= legOneDone+30 {
				if err := sim.SubmitDigLeg("LEG-B", digLegReq("g4-l2", "S1", "LINE"), "g4"); err != nil {
					t.Fatalf("submit leg 2: %v", err)
				}
				sim.SealDigGroup("g4")
				legTwoSubmitted = true
			}
		})
		if !settled {
			t.Fatalf("scenario did not settle")
		}
		if !tailAppended {
			t.Fatal("the retrieve never got its tail — the scenario did not exercise what it claims to")
		}
		if !dwelled {
			t.Error("the retrieve never waited outside the lane while the group worked — it cannot have " +
				"been held by anything, so this case proves nothing")
		}
		if enteredDuring {
			t.Error("the retrieve entered the lane while the dig GROUP was still working — the claim did " +
				"not span the gap between legs on the outbound side")
		}
	})
}

// ── E: two different digs on one lane ──────────────────────────────────────

// TestDigGroup_TwoDifferentDigsExcludeEachOther is case E, and it is a DEFECT
// FIXED rather than a property preserved.
//
// admitToLane's dig-hold block read `if myMode != "dig" && laneHasActiveDig(...)`.
// A foreign DIG therefore skipped the claim check entirely and was caught only by
// the occupant loop — which sees nothing while the holding dig is out on its
// parking leg. Two digs on one lane could interleave. It was latent only because
// nothing in the suite submitted two digs to one lane; groups make it reachable,
// so the guard is gone and the claim check applies to dig entrants of a different
// group too.
//
// RED AT 6fa583c2 (verified): restore the `myMode != "dig"` guard and DIG-2
// walks into a lane group g5a holds. Two things then fail, and the second is the
// more telling: dig-occupancy reports 'lane LANE is held by dig group "g5a" but
// the robot inside is [DIG-2=g5b]', and this case's own settle assertion fails
// because DIG-2 drops its bin at the mouth, which walls DIG-1's target and
// deadlocks the pair. A hole that lets two reshuffles interleave does not just
// break a rule; it strands both of them.
func TestDigGroup_TwoDifferentDigsExcludeEachOther(t *testing.T) {
	groupArms(t, func(t *testing.T, priorityOnly bool) {
		sim := groupSim(t, 4, priorityOnly)
		sim.PlaceBin("S0")
		sim.PlaceBin("S1")
		sim.PlaceBin("S2")
		for _, id := range []string{"DIG-1", "DIG-2"} {
			if err := sim.AddRobot(id, "AISLE"); err != nil {
				t.Fatalf("AddRobot %s: %v", id, err)
			}
		}
		// Two SEPARATE reshuffles, each a group of one, both wanting this lane.
		//
		// Dig 2 is INBOUND to the mouth slot, which keeps it genuinely REACHABLE
		// from tick zero. That matters: an outbound second dig would be buried
		// behind dig 1's bins and checkReachability would fire — correctly, and
		// before the exclusion property under test could be observed. The
		// contention being measured is for the LANE, so both actors must be able
		// to reach their work.
		if err := sim.Submit("DIG-1", digReq("g5a", "S0", "S1", "LINE"), true); err != nil {
			t.Fatalf("submit dig 1: %v", err)
		}
		if err := sim.Submit("DIG-2", digLegReq("g5b", "LINE", "S0"), true); err != nil {
			t.Fatalf("submit dig 2: %v", err)
		}

		var overlapped bool
		_, settled := runGroup(t, sim, 900, func(int) {
			d1, d2 := sim.robots["DIG-1"], sim.robots["DIG-2"]
			// The failure is not only "both inside at once" — it is EITHER dig
			// being inside while the OTHER's claim is live, which includes the
			// parking legs when the holder is outside the lane entirely.
			if d2 != nil && d2.pos.inLane() && sim.OrderActive("g5a") {
				overlapped = true
			}
			if d1 != nil && d1.pos.inLane() && sim.OrderActive("g5b") && !sim.OrderActive("g5a") {
				overlapped = true
			}
		})
		if !settled {
			t.Fatalf("two digs on one lane did not settle")
		}
		if overlapped {
			t.Error("two DIFFERENT digs interleaved on one lane: one entered while the other's claim was " +
				"still live. A foreign dig used to skip the claim check entirely (the `myMode != \"dig\"` " +
				"guard), and the occupant loop cannot see a holder that is out on a parking leg")
		}
	})
}

// ── F: soak across the geometry space ──────────────────────────────────────

// TestDigGroup_Soak is case F, in the shape of TestDig_Soak_HoldIsRobust: the
// group properties across depth 3–6 rather than one hand-built scene.
//
// wideLaneScene is parameterised by slot count, so depth 6 needs no fixture
// change — checked rather than assumed.
//
// Each seed splits a dig into per-slot legs across two robots, submits them with
// real gaps, and runs a foreign store at the lane throughout. The four live
// checkers (including dig-occupancy) run every tick for every seed; a violation
// anywhere fails the seed.
//
// MUTATION (proves non-vacuous): sealing each group at submission of its first
// leg. Seeds then fail on the store entering mid-group, and on the
// dig-occupancy checker once a later leg and the store are inside together.
func TestDigGroup_Soak(t *testing.T) {
	const seeds = 120
	var failures []int
	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		depth := 3 + rng.Intn(4) // 3..6 slots
		sim := New(wideLaneScene(t, depth), Options{Watchdog: 200, HopTicks: 3})
		sim.SetMouthGate(true)
		sim.SetPriorityOnly(rng.Intn(2) == 0) // both arms across the seed space

		nLegs := 2 + rng.Intn(2) // 2..3 legs, all shallower slots
		if nLegs > depth-1 {
			nLegs = depth - 1
		}
		for i := range nLegs {
			sim.PlaceBin(slotName(i))
		}
		for _, id := range []string{"LEG-A", "LEG-B", "STORE"} {
			if err := sim.AddRobot(id, "AISLE"); err != nil {
				t.Fatalf("seed %d AddRobot %s: %v", seed, id, err)
			}
		}

		group := fmt.Sprintf("soak-%d", seed)
		robots := []string{"LEG-A", "LEG-B"}
		submitted := 0
		submitLeg := func() {
			if submitted >= nLegs {
				return
			}
			r := robots[submitted%len(robots)]
			if sim.robots[r] == nil || !sim.robots[r].idle {
				return
			}
			id := fmt.Sprintf("%s-l%d", group, submitted)
			if err := sim.SubmitDigLeg(r, digLegReq(id, slotName(submitted), "LINE"), group); err != nil {
				t.Fatalf("seed %d submit %s: %v", seed, id, err)
			}
			submitted++
			if submitted == nLegs {
				sim.SealDigGroup(group)
			}
		}
		submitLeg()
		if err := sim.Submit("STORE", storeReq("store-1", "LINE", slotName(0)), false); err != nil {
			t.Fatalf("seed %d submit store: %v", seed, err)
		}

		bad := false
		maxInside := 0
		for tick := 0; tick < 1200; tick++ {
			for _, v := range sim.Tick() {
				t.Errorf("seed %d checker fired at tick %d: %s: %s", seed, sim.TickCount(), v.Checker, v.Detail)
				bad = true
			}
			if n := laneOccupancy(sim); n > maxInside {
				maxInside = n
			}
			submitLeg()
			if sim.AllIdle() && submitted == nLegs {
				break
			}
		}
		if maxInside > 1 {
			t.Errorf("seed %d: %d robots inside a dig-held lane at once", seed, maxInside)
			bad = true
		}
		if bad {
			failures = append(failures, seed)
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d/%d seeds failed: %v", len(failures), seeds, failures)
	}
	t.Logf("dig-group soak: %d seeds across depth 3-6 and both gate arms, one robot inside throughout", seeds)
}
