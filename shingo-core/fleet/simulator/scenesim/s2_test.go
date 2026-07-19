package scenesim

import (
	"fmt"
	"math/rand"
	"testing"

	"shingocore/domain"
)

// S2 — the mouth gate under test. The two S1 wounds are reproduced RED with the
// gate off (s1_test.go); here, with the gate ON, each turns GREEN — the measuring
// stick the seam exists to hit. Plus a mixed-mode exclusion and a seeded soak.

// wideLaneScene builds a single group + one lane of slotCount depth-ordered slots
// (S0..S{n-1}), a lineside source, and an aisle — for the soak's random streams.
func wideLaneScene(t *testing.T, slotCount int) *Scene {
	t.Helper()
	nodes := []domain.Node{
		{ID: 1, Name: "GRP", NodeTypeCode: "NGRP"},
		{ID: 2, Name: "LANE", NodeTypeCode: "LANE", ParentID: idPtr(1)},
		{ID: 100, Name: "LINE", NodeTypeCode: "STOR"},
		{ID: 101, Name: "AISLE"},
	}
	for i := range slotCount {
		nodes = append(nodes, domain.Node{
			ID: int64(10 + i), Name: fmt.Sprintf("S%d", i), ParentID: idPtr(2), Depth: depthPtr(i),
		})
	}
	sc, err := LoadScene(nodes)
	if err != nil {
		t.Fatalf("LoadScene: %v", err)
	}
	return sc
}

// TestS2_HeadOnGreenWithMouthGate: the Hopkinsville head-on — a swap parked in
// the lane and a store driving in — can no longer deadlock. The gate holds the
// store at the boundary while the swap is inside (capacity-1), so when the swap
// releases and exits, the store enters an empty lane and both complete. No
// mutual-block cycle ever forms.
//
// The swap works the DEEP slot and the store the SHALLOW one, so the swap's
// dropped bin never walls the store — the leader-walls-follower case is the
// separate §13.4 stale-binding wound (fixed by late binding, banked in P4), not
// the deadlock this test measures.
func TestS2_HeadOnGreenWithMouthGate(t *testing.T) {
	sc := plantScene(t)
	sim := New(sc, Options{Watchdog: 40})
	sim.SetMouthGate(true)
	_ = sim.AddRobot("SWAP", "AISLE")
	_ = sim.AddRobot("STORE", "AISLE")

	_ = sim.Submit("SWAP", carryStoreWait("swap-1", "LINE-IN", "A1-S2"), false) // deep
	vio := ticksCollect(sim, 80)
	_ = sim.Submit("STORE", storeReq("store-1", "LINE-IN", "A1-S1"), false) // shallow
	vio = append(vio, ticksCollect(sim, 80)...)

	if hasChecker(vio, "no-deadlock") {
		t.Fatalf("mouth gate must prevent the head-on deadlock; got: %+v", vio)
	}

	sim.ReleaseWait("swap-1")
	_, more, settled := sim.RunUntilIdle(500)
	vio = append(vio, more...)
	if !settled {
		t.Fatal("world did not settle after the swap released")
	}
	for _, v := range vio {
		t.Errorf("checker fired under the mouth gate (expected clean): %s: %s", v.Checker, v.Detail)
	}
}

// TestS2_AirBubbleGreenWithDeepestFirst: two stores bind slots at sourcing; with
// the gate the deep bind ENTERS FIRST (deepest-first admission), so the shallow
// store never walls it off — the §13.4 entry-order air bubble is gone.
func TestS2_AirBubbleGreenWithDeepestFirst(t *testing.T) {
	sc := plantScene(t)
	sim := New(sc, Options{Watchdog: 400})
	sim.SetMouthGate(true)
	_ = sim.AddRobot("SHALLOW", "AISLE")
	_ = sim.AddRobot("DEEP", "AISLE")

	// Submit shallow FIRST (the wound's bad order) — the gate must still enter the
	// deep bind first.
	_ = sim.Submit("SHALLOW", storeReq("shallow-1", "LINE-IN", "A1-S1"), false)
	_ = sim.Submit("DEEP", storeReq("deep-1", "LINE-IN", "A1-S2"), false)

	_, vio, settled := sim.RunUntilIdle(500)
	if !settled {
		t.Fatal("stores did not settle under the mouth gate")
	}
	for _, v := range vio {
		t.Errorf("checker fired under the mouth gate (expected clean): %s: %s", v.Checker, v.Detail)
	}
}

// TestS2_MixedModeExcluded: a dig holds a lane; a store trying to enter is kept
// out (different mode), so mode purity is never violated. After the dig releases,
// the store completes.
func TestS2_MixedModeExcluded(t *testing.T) {
	sc := plantScene(t)
	sim := New(sc, Options{Watchdog: 40})
	sim.SetMouthGate(true)
	_ = sim.AddRobot("DIG", "AISLE")
	_ = sim.AddRobot("STORE", "AISLE")

	_ = sim.Submit("DIG", carryStoreWait("dig-1", "LINE-IN", "A1-S2"), true) // dig=true, deep
	vio := ticksCollect(sim, 80)
	_ = sim.Submit("STORE", storeReq("store-1", "LINE-IN", "A1-S1"), false) // shallow
	vio = append(vio, ticksCollect(sim, 80)...)

	if hasChecker(vio, "mode-purity") {
		t.Fatalf("gate must keep the store out of the dig's lane; mode purity violated: %+v", vio)
	}
	if hasChecker(vio, "no-deadlock") {
		t.Fatalf("no deadlock expected under the gate: %+v", vio)
	}

	sim.ReleaseWait("dig-1")
	_, more, settled := sim.RunUntilIdle(500)
	vio = append(vio, more...)
	if !settled {
		t.Fatal("world did not settle after the dig released")
	}
	for _, v := range vio {
		t.Errorf("checker fired under the mouth gate (expected clean): %s: %s", v.Checker, v.Detail)
	}
}

// TestS2_Soak_DeepestFirstNoBubble is the seeded soak: for each seed, a random
// number of stores bind a random distinct set of slots and are submitted in a
// random order by fresh robots. Under the mouth gate every seed must settle with
// ZERO invariant violations — the standing pre-trial property (D73). A failing
// seed is reported for deterministic replay.
func TestS2_Soak_DeepestFirstNoBubble(t *testing.T) {
	const seeds = 200
	const slots = 4
	var failures []int
	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		sc := wideLaneScene(t, slots)
		sim := New(sc, Options{Watchdog: 120})
		sim.SetMouthGate(true)

		n := 1 + rng.Intn(slots)          // 1..slots concurrent stores
		targets := rng.Perm(slots)[:n]    // distinct slots
		for i, slotIdx := range targets { // submitted in this (random) order
			rid := fmt.Sprintf("R%d", i)
			if err := sim.AddRobot(rid, "AISLE"); err != nil {
				t.Fatalf("seed %d AddRobot: %v", seed, err)
			}
			req := storeReq(fmt.Sprintf("o%d-%d", seed, i), "LINE", fmt.Sprintf("S%d", slotIdx))
			if err := sim.Submit(rid, req, false); err != nil {
				t.Fatalf("seed %d Submit: %v", seed, err)
			}
		}

		_, vios, settled := sim.RunUntilIdle(4000)
		if !settled || len(vios) > 0 {
			failures = append(failures, seed)
			if len(failures) <= 3 {
				t.Logf("seed %d FAILED: settled=%v n=%d targets=%v violations=%+v",
					seed, settled, n, targets, vios)
			}
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d/%d seeds violated an invariant under the mouth gate; first: %v",
			len(failures), seeds, firstN(failures, 10))
	}
	t.Logf("soak clean: %d seeds, lane of %d slots, random store counts + orders — no violations",
		seeds, slots)
}

func firstN(xs []int, n int) []int {
	if len(xs) < n {
		return xs
	}
	return xs[:n]
}
