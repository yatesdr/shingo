package scenesim

import (
	"fmt"
	"math/rand"
	"testing"

	"shingocore/fleet"
)

// dig_test.go — INCREMENT 0 (dig mechanics): the retrieve path with contention.
//
// Now that outbound pickup physics + robot-carrying state + the wall exemption for
// a pickup target are in the harness, a DIG — a reshuffle that extracts a bin
// BURIED behind shallower blockers — is expressible. These scenarios prove the
// three properties the dig's ModeDig mouth row exists to guarantee, modeled over
// real single-file lane physics.
//
// HOW A DIG IS MODELED HERE vs IN PRODUCTION. Production runs a dig as a COMPOUND
// of separate 2-block child orders (unbury the blocker to a shuffle slot, then
// retrieve the target to the line), held lane-exclusive for the whole compound by
// a durable 'dig' mouth reservation. The harness models the same work as ONE
// multi-leg order flagged Dig=true: [pickup@blocker, dropoff@shuffle, pickup@target,
// dropoff@line]. The PHYSICS under test — single-file unbury, mode-exclusive hold
// across the out-and-back legs, release on completion — is identical; the harness
// collapses the compound into one order because it is testing the lane physics, not
// the child-dispatch machinery (that is asserted in the dispatch docker battery).

// digReq builds a two-leg dig: lift the shallow blocker out to the line (clearing
// the path), return for the buried target, deliver it to the line. Submitted with
// dig=true so it holds the lane mode-exclusive (ModeDig).
//
// The blocker goes to LINE (the harness's only plain staging node); production
// would relocate it to a shuffle slot and leave it there permanently (no restock).
// For the lane physics that distinction is immaterial — what matters is that the
// blocker leaves its slot so the deeper target becomes reachable.
func digReq(id, blockerSlot, targetSlot, dest string) fleet.CreateOrderRequest {
	return fleet.CreateOrderRequest{
		OrderID: id,
		Blocks: []fleet.OrderBlock{
			{BlockID: id + "-u1", Location: blockerSlot, BinTask: "JackLoad"},
			{BlockID: id + "-u2", Location: dest, BinTask: "JackUnload"},
			{BlockID: id + "-r1", Location: targetSlot, BinTask: "JackLoad"},
			{BlockID: id + "-r2", Location: dest, BinTask: "JackUnload"},
		},
		Complete: true,
	}
}

// TestDig_LiftsBlockerThenTarget is the existence proof: a dig against a buried
// target actually extracts it. The blocker sits shallow (S0), the target behind it
// (S1). Single-file, S1 is unreachable until S0 is gone. The dig must lift S0 out,
// return for S1, and deliver S1 — settling clean with the target gone.
func TestDig_LiftsBlockerThenTarget(t *testing.T) {
	sim := newGatedSim(t, 3, 0)
	sim.PlaceBin("S0") // blocker, shallow
	sim.PlaceBin("S1") // target, buried behind the blocker
	if err := sim.AddRobot("DIGGER", "AISLE"); err != nil {
		t.Fatalf("AddRobot: %v", err)
	}
	if err := sim.Submit("DIGGER", digReq("dig-1", "S0", "S1", "LINE"), true); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ticks, vios, settled := sim.RunUntilIdle(600)
	if !settled {
		t.Fatalf("dig did not settle (ran %d ticks, busy=%d)", ticks, sim.BusyCount())
	}
	for _, v := range vios {
		t.Errorf("dig fired a checker: %s: %s", v.Checker, v.Detail)
	}
	if sim.HasBin("S1") {
		t.Error("the buried target is still in its slot — the dig never extracted it")
	}
	if sim.HasBin("S0") {
		t.Error("the blocker is still in its slot — the dig never cleared the path")
	}
	t.Logf("2-bin dig settled in %d ticks, both bins extracted, zero violations", ticks)
}

// TestDig_ExcludesStoreAcrossLegs is the safety property the dig hold exists for.
//
// A dig is a multi-leg order: it leaves the lane to park a blocker, then RETURNS for
// the buried target. During that out-and-back leg the lane is momentarily empty of
// the dig robot. A store submitted to a shallow slot must NOT slip in then — the dig
// has not finished, its ModeDig hold spans the whole compound, and if the store
// enters the two will collide when the dig returns. This is the failure the dig-hold
// block in admitToLane prevents: an active dig claims its lane for its order's
// lifetime, not just the ticks its robot is physically inside.
//
// Asserted twice: no mode-purity violation ever (the store never shares the dig's
// lane), and the store is never inside the lane while the dig is still active. The
// store completes cleanly AFTER the dig finishes.
func TestDig_ExcludesStoreAcrossLegs(t *testing.T) {
	sim := newGatedSim(t, 3, 0)
	sim.PlaceBin("S0") // blocker
	sim.PlaceBin("S1") // dig target (buried)
	if err := sim.AddRobot("DIGGER", "AISLE"); err != nil {
		t.Fatalf("AddRobot DIGGER: %v", err)
	}
	if err := sim.AddRobot("STORE", "AISLE"); err != nil {
		t.Fatalf("AddRobot STORE: %v", err)
	}
	if err := sim.Submit("DIGGER", digReq("dig-1", "S0", "S1", "LINE"), true); err != nil {
		t.Fatalf("Submit dig: %v", err)
	}
	// Store wants S0 (which the dig will free). It must wait for the WHOLE dig.
	if err := sim.Submit("STORE", storeReq("store-1", "LINE", "S0"), false); err != nil {
		t.Fatalf("Submit store: %v", err)
	}

	var storeEnteredDuringDig bool
	var storeCompleted bool
	var digDoneTick int
	for tick := 0; tick < 400; tick++ {
		for _, v := range sim.Tick() {
			t.Errorf("dig+store fired a checker at tick %d: %s: %s", sim.TickCount(), v.Checker, v.Detail)
		}
		digActive := sim.OrderActive("dig-1")
		if !digActive && digDoneTick == 0 {
			digDoneTick = sim.TickCount()
		}
		sr := sim.robots["STORE"]
		if sr != nil && sr.pos.inLane() && digActive {
			storeEnteredDuringDig = true
		}
		if digDoneTick > 0 && !sim.OrderActive("store-1") && !sim.HasBin("S0") {
			// store placed into S0 after the dig freed it — but the dig dropped S0's
			// original bin at LINE, so the store re-fills the now-empty shallow slot.
		}
		if sim.AllIdle() {
			storeCompleted = true
			break
		}
	}
	if !storeCompleted {
		t.Fatalf("store never completed after the dig (dig done @%d)", digDoneTick)
	}
	if storeEnteredDuringDig {
		t.Error("the store entered the lane while the dig was still active — the dig " +
			"hold failed to keep it out across the out-and-back leg")
	}
	// The store must have gone in AFTER the dig finished, never during.
	storeRobot := sim.robots["STORE"]
	if storeRobot == nil || !storeRobot.idle {
		t.Error("store did not reach idle")
	}
	t.Logf("dig+store: dig done @%d, store held out for the whole dig, then completed; settled @%d",
		digDoneTick, sim.TickCount())
}

// TestDig_ReleasesLaneOnCompletion confirms the hold is not sticky: once the dig
// finishes, a subsequent store enters the now-free lane and completes. (A hold that
// never cleared would leave the lane permanently locked — the inverse failure.)
func TestDig_ReleasesLaneOnCompletion(t *testing.T) {
	sim := newGatedSim(t, 3, 0)
	sim.PlaceBin("S0")
	sim.PlaceBin("S1")
	if err := sim.AddRobot("DIGGER", "AISLE"); err != nil {
		t.Fatalf("AddRobot DIGGER: %v", err)
	}
	if err := sim.AddRobot("STORE", "AISLE"); err != nil {
		t.Fatalf("AddRobot STORE: %v", err)
	}
	if err := sim.Submit("DIGGER", digReq("dig-1", "S0", "S1", "LINE"), true); err != nil {
		t.Fatalf("Submit dig: %v", err)
	}
	// Let the dig finish first.
	_, vios, settled := sim.RunUntilIdle(400)
	if !settled {
		t.Fatalf("dig did not settle")
	}
	for _, v := range vios {
		t.Errorf("dig fired a checker: %s: %s", v.Checker, v.Detail)
	}
	// Now a store into a shallow slot must enter cleanly — the dig's hold is gone.
	if err := sim.Submit("STORE", storeReq("store-1", "LINE", "S0"), false); err != nil {
		t.Fatalf("Submit store after dig: %v", err)
	}
	ticks, vios2, settled2 := sim.RunUntilIdle(400)
	if !settled2 {
		t.Fatalf("store after dig did not settle (ran %d ticks)", ticks)
	}
	for _, v := range vios2 {
		t.Errorf("store-after-dig fired a checker: %s: %s", v.Checker, v.Detail)
	}
	if !sim.HasBin("S0") {
		t.Error("the store never placed into S0 — the dig's hold may not have released")
	}
	t.Logf("dig released the lane on completion; follow-on store settled in %d ticks", ticks)
}

// TestDig_HoldIsArmAgnostic confirms the dig hold excludes a store under BOTH gate
// arms, not only the priority-only arm the other dig tests default to (newGatedSim
// sets priorityOnly=true). The production pilot gates with deepest-first admission
// (priorityOnly=false) — the arm where the deepest-first hold is ACTIVE — so the dig
// hold must keep the store out there too. Same property, other arm.
func TestDig_HoldIsArmAgnostic(t *testing.T) {
	for _, arm := range []struct {
		name         string
		priorityOnly bool
	}{
		{"priority-only", true},
		{"deepest-first", false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			sc := wideLaneScene(t, 3)
			sim := New(sc, Options{Watchdog: 200, HopTicks: 3})
			sim.SetMouthGate(true)
			sim.SetPriorityOnly(arm.priorityOnly)
			sim.PlaceBin("S0")
			sim.PlaceBin("S1")
			if err := sim.AddRobot("DIGGER", "AISLE"); err != nil {
				t.Fatalf("AddRobot DIGGER: %v", err)
			}
			if err := sim.AddRobot("STORE", "AISLE"); err != nil {
				t.Fatalf("AddRobot STORE: %v", err)
			}
			if err := sim.Submit("DIGGER", digReq("dig-1", "S0", "S1", "LINE"), true); err != nil {
				t.Fatalf("Submit dig: %v", err)
			}
			if err := sim.Submit("STORE", storeReq("store-1", "LINE", "S0"), false); err != nil {
				t.Fatalf("Submit store: %v", err)
			}
			storeEnteredDuringDig := false
			for tick := 0; tick < 400; tick++ {
				for _, v := range sim.Tick() {
					t.Errorf("%s arm fired a checker at tick %d: %s: %s", arm.name, sim.TickCount(), v.Checker, v.Detail)
				}
				if sr := sim.robots["STORE"]; sr != nil && sr.pos.inLane() && sim.OrderActive("dig-1") {
					storeEnteredDuringDig = true
				}
				if sim.AllIdle() {
					break
				}
			}
			if storeEnteredDuringDig {
				t.Errorf("%s arm: the store entered the lane while the dig was still active", arm.name)
			}
			if sim.HasBin("S1") {
				t.Errorf("%s arm: the buried target was not extracted", arm.name)
			}
			t.Logf("%s arm: dig held, store kept out, target extracted", arm.name)
		})
	}
}

// TestDig_Soak_HoldIsRobust is the seeded soak: across randomized lane depth,
// blocker counts, and a concurrent store trying to enter DURING the dig, every
// seed must settle with zero violations, the target extracted, and the store never
// having shared the lane with the active dig. A dig hold that only works on one
// hand-built geometry would fail here; this is the standing property the dig
// mechanics must hold across the geometry space the plants actually present.
//
// The store targets a slot the dig will FREE (so it has a reason to race in), at a
// random tick during the dig. The dig targets the deepest slot, behind 1..depth-1
// blockers. Geometry is bounded so the dig always has a reachable target.
func TestDig_Soak_HoldIsRobust(t *testing.T) {
	const seeds = 200
	var failures []int
	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		depth := 3 + rng.Intn(3) // 3..5 slots
		sc := wideLaneScene(t, depth)
		sim := New(sc, Options{Watchdog: 200, HopTicks: 3})
		sim.SetMouthGate(true)
		sim.SetPriorityOnly(true)

		// Target is the deepest slot; 1..depth-1 blockers fill the slots in front.
		targetIdx := depth - 1
		nBlockers := 1 + rng.Intn(targetIdx) // at least 1, up to all in front
		blockerIdxs := rng.Perm(targetIdx)[:nBlockers]
		for _, i := range blockerIdxs {
			sim.PlaceBin(slotName(i))
		}
		sim.PlaceBin(slotName(targetIdx))

		if err := sim.AddRobot("DIGGER", "AISLE"); err != nil {
			t.Fatalf("seed %d AddRobot DIGGER: %v", seed, err)
		}
		// Dig: unbury each blocker (shallowest first) then retrieve the target.
		sortedBlockers := append([]int(nil), blockerIdxs...)
		// shallowest-first unbury (production's front-to-back order)
		for i := 0; i < len(sortedBlockers); i++ {
			for j := i + 1; j < len(sortedBlockers); j++ {
				if sortedBlockers[j] < sortedBlockers[i] {
					sortedBlockers[i], sortedBlockers[j] = sortedBlockers[j], sortedBlockers[i]
				}
			}
		}
		var blocks []fleet.OrderBlock
		bid := 0
		for _, bi := range sortedBlockers {
			blocks = append(blocks,
				fleet.OrderBlock{BlockID: digBlockID(&bid), Location: slotName(bi), BinTask: "JackLoad"},
				fleet.OrderBlock{BlockID: digBlockID(&bid), Location: "LINE", BinTask: "JackUnload"})
		}
		blocks = append(blocks,
			fleet.OrderBlock{BlockID: digBlockID(&bid), Location: slotName(targetIdx), BinTask: "JackLoad"},
			fleet.OrderBlock{BlockID: digBlockID(&bid), Location: "LINE", BinTask: "JackUnload"})
		if err := sim.Submit("DIGGER", fleet.CreateOrderRequest{OrderID: digOrderID(seed), Blocks: blocks, Complete: true}, true); err != nil {
			t.Fatalf("seed %d Submit dig: %v", seed, err)
		}

		// A store races for the SHALLOWEST blocker slot — the first one the dig frees,
		// so once the dig hold releases the slot is genuinely reachable (a store bound
		// for a deeper still-buried slot would be walled by a blocker the dig hasn't
		// lifted yet, and the reachability checker would rightly fire — that is a bad
		// bind, not a dig-mechanics failure). Submitted at a random tick during the dig.
		storeSlotIdx := sortedBlockers[0]
		storeSubmitted := false
		storeEnterDuringDig := false
		var vios []Violation
		for tick := 0; tick < 800; tick++ {
			if !storeSubmitted && tick == 1+rng.Intn(6) {
				if err := sim.AddRobot("STORE", "AISLE"); err == nil {
					_ = sim.Submit("STORE", storeReq("store-"+digOrderID(seed), "LINE", slotName(storeSlotIdx)), false)
				}
				storeSubmitted = true
			}
			vios = append(vios, sim.Tick()...)
			digActive := sim.OrderActive(digOrderID(seed))
			if sr := sim.robots["STORE"]; sr != nil && sr.pos.inLane() && digActive {
				storeEnterDuringDig = true
			}
			if sim.AllIdle() {
				break
			}
		}
		settled := sim.AllIdle()
		targetGone := !sim.HasBin(slotName(targetIdx))
		if !settled || len(vios) > 0 || !targetGone || storeEnterDuringDig {
			failures = append(failures, seed)
			if len(failures) <= 3 {
				t.Logf("seed %d FAILED: settled=%v vios=%d targetGone=%v storeEnterDuringDig=%v (depth=%d blockers=%v)",
					seed, settled, len(vios), targetGone, storeEnterDuringDig, depth, sortedBlockers)
			}
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d/%d dig seeds failed; first: %v", len(failures), seeds, firstN(failures, 10))
	}
	t.Logf("dig soak clean: %d seeds, depth 3..5, 1..%d blockers, concurrent store — hold held, target always extracted",
		seeds, 4)
}

func slotName(depthIdx int) string { return fmt.Sprintf("S%d", depthIdx) }
func digOrderID(seed int) string   { return fmt.Sprintf("dig-soak-%d", seed) }
func digBlockID(bid *int) string {
	*bid++
	return fmt.Sprintf("dig-b%d", *bid)
}
