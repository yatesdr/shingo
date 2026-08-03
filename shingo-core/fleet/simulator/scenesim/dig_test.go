package scenesim

import (
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
