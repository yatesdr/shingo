package scenesim

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// Stage-1 wall experiment (harness only): does a shallower store WALL a deeper one
// under the CURRENT production arms as landed — the mode-based mouth gate plus the
// RDS priority HINT — with NO deepest-first admission hold? The hold is exactly
// what tiered entry would add; SetPriorityOnly leaves it out so we test priority
// alone. Priority is modeled realistically as a START-order advantage (the deeper
// store is submitted first), never as control over an already-moving robot; travel
// distance (SetRobotApproach) decides who actually reaches the mouth first.
//
// Two independent wall signals: the reachability checker firing (a bin sits in a
// shallower slot, ahead of an in-flight robot's deeper target — unreachable in a
// single-file lane), and walled stores = robots left stuck at the end (BusyCount).

const wallHop = 3 // matches Options default HopTicks; used to size staggers in hops

// storeSpec is one store in a wall run: its target slot, the robot's coarse travel
// distance (approach hops), and the tick it is submitted (priority head start =
// deeper submitted earlier).
type storeSpec struct {
	id       string
	slot     string
	approach int
	submitAt int
}

// runWall drives specs on a fresh `slots`-deep lane (preFill slots already holding
// bins) under the mouth gate; priorityOnly selects production-as-landed vs the
// deepest-first fix. Robots are added up front and each store is submitted at its
// submitAt tick. Returns the number of walled (stuck) stores, whether the world
// settled, and the violations seen.
func runWall(t *testing.T, slots int, preFill []string, priorityOnly bool, specs []storeSpec, maxTicks int) (walledStores int, settled bool, vios []Violation) {
	t.Helper()
	sc := wideLaneScene(t, slots)
	sim := New(sc, Options{Watchdog: 80})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(priorityOnly)
	for _, s := range preFill {
		sim.PlaceBin(s)
	}
	for _, sp := range specs {
		if err := sim.AddRobot(sp.id, "AISLE"); err != nil {
			t.Fatalf("AddRobot %s: %v", sp.id, err)
		}
	}
	pending := append([]storeSpec(nil), specs...)
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].submitAt < pending[j].submitAt })

	for tick := range maxTicks {
		for len(pending) > 0 && pending[0].submitAt <= tick {
			sp := pending[0]
			pending = pending[1:]
			sim.SetRobotApproach(sp.id, sp.approach)
			if err := sim.Submit(sp.id, storeReq(sp.id, "LINE", sp.slot), false); err != nil {
				t.Fatalf("Submit %s: %v", sp.id, err)
			}
		}
		vios = append(vios, sim.Tick()...)
		if len(pending) == 0 && sim.AllIdle() {
			return 0, true, vios
		}
	}
	return sim.BusyCount(), false, vios
}

// TestWall_OwnerScenario_PriorityOnly is the owner's exact case: 4-deep lane, the
// two deepest slots full; store A → slot 2 (S1), dispatched FAR and FIRST (its
// priority head start); store B → slot 1 (S0), spawned moments later with a robot
// CLOSER to the lane. Under priority-only, B reaches the mouth first and drops in
// front of A — B walls A. This proves the wall is a real failure under the arms as
// landed.
func TestWall_OwnerScenario_PriorityOnly(t *testing.T) {
	specs := []storeSpec{
		{id: "A-deep", slot: "S1", approach: 6, submitAt: 0},              // deep target, far, head start
		{id: "B-shallow", slot: "S0", approach: 1, submitAt: 2 * wallHop}, // shallow target, close, later
	}
	walled, settled, vios := runWall(t, 4, []string{"S2", "S3"}, true, specs, 400)
	if walled == 0 || !hasChecker(vios, "reachability") {
		t.Errorf("expected B to WALL A under priority-only (no deepest-first hold); walledStores=%d reachability=%v settled=%v",
			walled, hasChecker(vios, "reachability"), settled)
	}
	t.Logf("owner scenario, PRIORITY-ONLY: walledStores=%d reachability=%v settled=%v (A far+first, B close+later → B wins the race)",
		walled, hasChecker(vios, "reachability"), settled)
}

// TestWall_OwnerScenario_DeepestFirstPrevents runs the SAME scenario with the
// deepest-first admission hold on (what tiered entry would add): B is held at the
// mouth until the deeper A has entered, so no wall forms and the world settles.
// This is the green target the fix must hit.
func TestWall_OwnerScenario_DeepestFirstPrevents(t *testing.T) {
	specs := []storeSpec{
		{id: "A-deep", slot: "S1", approach: 6, submitAt: 0},
		{id: "B-shallow", slot: "S0", approach: 1, submitAt: 2 * wallHop},
	}
	walled, settled, _ := runWall(t, 4, []string{"S2", "S3"}, false, specs, 400)
	if walled > 0 || !settled {
		t.Errorf("deepest-first admission should prevent the wall and settle; walledStores=%d settled=%v", walled, settled)
	}
	t.Logf("owner scenario, DEEPEST-FIRST (the fix): walledStores=%d settled=%v (B held for A → no wall)", walled, settled)
}

// TestWall_SoakRate measures FREQUENCY, not just possibility: over 200 seeds with
// randomized lane pre-fill, per-robot travel distance, and priority head-start
// staggers, it counts how many stores get walled under priority-only versus under
// the deepest-first fix (which must be zero). It reports walls per 1,000 stores and
// how often the priority hint alone got the whole seed right.
func TestWall_SoakRate(t *testing.T) {
	const seeds = 200
	const slots = 4
	var walledStores, cleanSeeds, totalStores, fixWalledStores int

	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))

		// Fill the deepest f slots (0..slots-2), leaving >=2 empty for a race.
		f := rng.Intn(slots - 1)
		var preFill []string
		for i := slots - f; i < slots; i++ {
			preFill = append(preFill, fmt.Sprintf("S%d", i))
		}

		// Empties = indices 0 .. nEmpty-1. Stores target them, submitted DEEPEST
		// FIRST (the priority head start), with a per-step stagger of 0..2 hops.
		// Each robot gets a random approach distance (1..8 hops) — the confound
		// that can let a shallower-but-closer robot beat the head start.
		stagger := rng.Intn(3) * wallHop
		nEmpty := slots - f
		var specs []storeSpec
		for k := range nEmpty {
			depthIdx := (nEmpty - 1) - k // deepest empty first
			specs = append(specs, storeSpec{
				id:       fmt.Sprintf("R%d", depthIdx),
				slot:     fmt.Sprintf("S%d", depthIdx),
				approach: 1 + rng.Intn(8),
				submitAt: k * stagger,
			})
		}
		totalStores += len(specs)

		if w, _, _ := runWall(t, slots, preFill, true, specs, 500); w > 0 {
			walledStores += w
		} else {
			cleanSeeds++
		}
		if w, _, _ := runWall(t, slots, preFill, false, specs, 500); w > 0 {
			fixWalledStores += w
		}
	}

	rate := float64(walledStores) * 1000 / float64(totalStores)
	t.Logf("PRIORITY-ONLY soak: %d walled stores of %d total = %.1f walls per 1,000 stores; %d/%d seeds fully clean (priority hint alone got the order right)",
		walledStores, totalStores, rate, cleanSeeds, seeds)
	t.Logf("DEEPEST-FIRST (tiered entry) over the SAME %d configs: %d walled stores — the fix", seeds, fixWalledStores)
	if fixWalledStores > 0 {
		t.Errorf("deepest-first admission must eliminate the wall, but %d stores still walled", fixWalledStores)
	}
}

// TestWall_PressPairBaseline records the baseline the build must not regress: two
// same-kind stores dispatched TOGETHER co-occupy and complete with no dispatch
// hold. It also measures what a NAIVE deepest-first hold would cost them (the
// shallow store waits at the mouth) — the latency Tier 1 (same-origin exemption)
// must avoid. Stage 3 asserts Tier 1 keeps the priority-only figure.
func TestWall_PressPairBaseline(t *testing.T) {
	specs := []storeSpec{
		{id: "PRESS-L", slot: "S1", approach: 1, submitAt: 0},
		{id: "PRESS-R", slot: "S0", approach: 1, submitAt: 0},
	}
	settleTicks := func(priorityOnly bool) int {
		sc := wideLaneScene(t, 4)
		sim := New(sc, Options{Watchdog: 200})
		sim.SetMouthGate(true)
		sim.SetPriorityOnly(priorityOnly)
		for _, sp := range specs {
			_ = sim.AddRobot(sp.id, "AISLE")
			sim.SetRobotApproach(sp.id, sp.approach)
			_ = sim.Submit(sp.id, storeReq(sp.id, "LINE", sp.slot), false)
		}
		ticks, vios, settled := sim.RunUntilIdle(400)
		if !settled {
			t.Fatalf("press pair did not settle (priorityOnly=%v)", priorityOnly)
		}
		for _, v := range vios {
			t.Errorf("press pair (priorityOnly=%v) fired a checker: %s: %s", priorityOnly, v.Checker, v.Detail)
		}
		return ticks
	}

	base := settleTicks(true)  // production-as-landed baseline: co-dispatch, no hold
	held := settleTicks(false) // naive deepest-first: shallow store waits at the mouth
	t.Logf("press-pair baseline (co-dispatch, priority-only): settle=%d ticks", base)
	t.Logf("press-pair under NAIVE deepest-first (no same-origin exemption): settle=%d ticks (+%d) — the latency Tier 1 must avoid",
		held, held-base)
}
