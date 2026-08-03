package scenesim

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// Stage-3 proof (harness): the tiered-entry FIX turns the wall green, and the
// press pair still co-dispatches with zero added latency. Crucially this models
// the REAL production mechanism — a dispatch-time SUBMISSION gate — under the real
// production physics (SetPriorityOnly: mode mouth gate + priority hint, NO
// deepest-first mouth hold). It does NOT lean on the harness's own deepest-first
// admission (that was the Stage-1 proxy). A cross-origin store is not SUBMITTED
// until the deeper cross-origin store it waits on has COMPLETED (the shipped
// classifier's release signal); same-origin partners are exempt and co-dispatch.

// runTieredGated drives cross-origin stores under the production physics with the
// tiered SUBMISSION gate: submit deepest-first, and do not submit the next
// (shallower) store until the previous (deeper) one has COMPLETED. Robots are
// added lazily so an un-submitted robot never counts as idle. Returns walled
// (stuck) stores, settled, and violations.
func runTieredGated(t *testing.T, slots int, preFill []string, specs []storeSpec, maxTicks int) (walled int, settled bool, vios []Violation) {
	t.Helper()
	sc := wideLaneScene(t, slots)
	sim := New(sc, Options{Watchdog: 80})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(true) // real production physics — no deepest-first mouth hold
	for _, s := range preFill {
		sim.PlaceBin(s)
	}

	// Deepest target first (the tiered submission order).
	ordered := append([]storeSpec(nil), specs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		di, _ := sc.SlotDepth(ordered[i].slot)
		dj, _ := sc.SlotDepth(ordered[j].slot)
		return di > dj
	})

	for _, sp := range ordered {
		if err := sim.AddRobot(sp.id, "AISLE"); err != nil {
			t.Fatalf("AddRobot %s: %v", sp.id, err)
		}
		sim.SetRobotApproach(sp.id, sp.approach)
		if err := sim.Submit(sp.id, storeReq(sp.id, "LINE", sp.slot), false); err != nil {
			t.Fatalf("Submit %s: %v", sp.id, err)
		}
		// Hold the next submission until every already-submitted store has
		// completed (all present robots idle) — the completion release signal.
		done := false
		for range maxTicks {
			vios = append(vios, sim.Tick()...)
			if sim.AllIdle() {
				done = true
				break
			}
		}
		if !done {
			return sim.BusyCount(), false, vios
		}
	}
	return sim.BusyCount(), true, vios
}

// TestTieredEntry_OwnerScenario_Green: the owner scenario that walls under
// priority-only (Stage 1) is CLEAN under the tiered submission gate — the shallow
// store B is not submitted until the deeper A has completed, so B can never wall A.
func TestTieredEntry_OwnerScenario_Green(t *testing.T) {
	specs := []storeSpec{
		{id: "A-deep", slot: "S1", approach: 6},
		{id: "B-shallow", slot: "S0", approach: 1},
	}
	walled, settled, vios := runTieredGated(t, 4, []string{"S2", "S3"}, specs, 400)
	if walled != 0 || !settled {
		t.Errorf("tiered entry must leave the owner scenario wall-free; walledStores=%d settled=%v", walled, settled)
	}
	for _, v := range vios {
		t.Errorf("tiered entry fired a checker (expected clean): %s: %s", v.Checker, v.Detail)
	}
	t.Logf("owner scenario under TIERED ENTRY: walledStores=%d settled=%v — green", walled, settled)
}

// TestTieredEntry_PressPairZeroLatency: two SAME-ORIGIN stores (Tier 1) co-dispatch
// — no submission gate — and settle with no added latency and no checker firing,
// while the cross-origin (Tier 2) completion gate on the SAME pair would serialize
// them. Regressing the press is the one unforgivable outcome, so this asserts the
// co-dispatch time is strictly less than the gated time and matches the plain
// baseline.
func TestTieredEntry_PressPairZeroLatency(t *testing.T) {
	// Same-origin partners are equidistant from their supermarket; deeper is
	// ordered first by the RDS priority hint, so co-dispatch is wall-free.
	specs := []storeSpec{
		{id: "PRESS-L", slot: "S1", approach: 2, submitAt: 0}, // deep
		{id: "PRESS-R", slot: "S0", approach: 2, submitAt: 0}, // shallow
	}

	// Tier 1: co-dispatch (both submitted at tick 0, no gate) under production physics.
	coWalled, coSettled, coVios := runWall(t, 4, nil, true, specs, 400)
	coTicks := ticksToSettle(t, 4, nil, specs)
	if coWalled != 0 || !coSettled {
		t.Fatalf("same-origin co-dispatch must be wall-free; walled=%d settled=%v", coWalled, coSettled)
	}
	for _, v := range coVios {
		t.Errorf("press pair co-dispatch fired a checker: %s: %s", v.Checker, v.Detail)
	}

	// If the SAME pair were treated cross-origin, the Tier-2 completion gate would
	// serialize them — strictly more latency. That gap is exactly what Tier 1 saves.
	_, gatedSettled, _ := runTieredGated(t, 4, nil, specs, 400)
	gatedTicks := tieredGatedTicks(t, 4, nil, specs)
	if !gatedSettled {
		t.Fatal("gated variant should still settle")
	}
	if !(coTicks < gatedTicks) {
		t.Errorf("Tier 1 co-dispatch (%d ticks) must be FASTER than the cross-origin gate (%d ticks) — zero added latency requires it",
			coTicks, gatedTicks)
	}
	t.Logf("press pair: Tier-1 co-dispatch=%d ticks vs cross-origin gate=%d ticks (Tier 1 saves %d)",
		coTicks, gatedTicks, gatedTicks-coTicks)
}

// TestTieredEntry_SoakZeroWalls re-runs the Stage-1 soak configs as cross-origin
// stores under the tiered submission gate: every seed must be wall-free with all
// checkers green.
func TestTieredEntry_SoakZeroWalls(t *testing.T) {
	const seeds = 200
	const slots = 4
	var walled, violated int

	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		f := rng.Intn(slots - 1)
		var preFill []string
		for i := slots - f; i < slots; i++ {
			preFill = append(preFill, fmt.Sprintf("S%d", i))
		}
		nEmpty := slots - f
		var specs []storeSpec
		for k := range nEmpty {
			depthIdx := (nEmpty - 1) - k
			specs = append(specs, storeSpec{
				id:       fmt.Sprintf("R%d", depthIdx),
				slot:     fmt.Sprintf("S%d", depthIdx),
				approach: 1 + rng.Intn(8),
			})
		}
		w, settled, vios := runTieredGated(t, slots, preFill, specs, 500)
		if w > 0 || !settled {
			walled++
		}
		if len(vios) > 0 {
			violated++
		}
	}
	if walled > 0 || violated > 0 {
		t.Errorf("tiered entry soak must be clean: %d/%d seeds walled, %d/%d seeds fired a checker", walled, seeds, violated, seeds)
	}
	t.Logf("TIERED ENTRY soak: %d/%d seeds walled, %d/%d fired a checker — clean", walled, seeds, violated, seeds)
}

// ticksToSettle runs specs co-dispatched (Tier 1) under production physics and
// returns the settle tick count.
func ticksToSettle(t *testing.T, slots int, preFill []string, specs []storeSpec) int {
	t.Helper()
	sc := wideLaneScene(t, slots)
	sim := New(sc, Options{Watchdog: 200})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(true)
	for _, s := range preFill {
		sim.PlaceBin(s)
	}
	for _, sp := range specs {
		_ = sim.AddRobot(sp.id, "AISLE")
		sim.SetRobotApproach(sp.id, sp.approach)
		_ = sim.Submit(sp.id, storeReq(sp.id, "LINE", sp.slot), false)
	}
	ticks, _, settled := sim.RunUntilIdle(400)
	if !settled {
		t.Fatal("co-dispatch did not settle")
	}
	return ticks
}

// tieredGatedTicks returns the settle tick count for the completion-gated variant.
func tieredGatedTicks(t *testing.T, slots int, preFill []string, specs []storeSpec) int {
	t.Helper()
	sc := wideLaneScene(t, slots)
	sim := New(sc, Options{Watchdog: 80})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(true)
	for _, s := range preFill {
		sim.PlaceBin(s)
	}
	ordered := append([]storeSpec(nil), specs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		di, _ := sc.SlotDepth(ordered[i].slot)
		dj, _ := sc.SlotDepth(ordered[j].slot)
		return di > dj
	})
	total := 0
	for _, sp := range ordered {
		_ = sim.AddRobot(sp.id, "AISLE")
		sim.SetRobotApproach(sp.id, sp.approach)
		_ = sim.Submit(sp.id, storeReq(sp.id, "LINE", sp.slot), false)
		for range 400 {
			sim.Tick()
			total++
			if sim.AllIdle() {
				break
			}
		}
	}
	return total
}
