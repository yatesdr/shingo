package scenesim

import (
	"testing"

	"shingocore/fleet"
)

// Harness for the UNIFORM gated shape (increment 3).
//
// Production ships EVERY lane-bound store on a gate_choreography group as an
// unsealed waybill ending at the lane's wait point, and appends the dropoff tail
// when the lane is safe — immediately, back to back with the create, when it
// already is. There is no bypass class, so the open and contended cases differ
// only in WHEN the tail is appended, never in the shape of what was created.
//
// These scenarios model exactly that, using the real deferred tail (Submit the
// unsealed pair, then AppendBlocks) rather than a pre-known plan behind a flag —
// so what is under test is the design, not a mock of it.

// gatedStoreReq is what the valve CREATES: [pickup@source, wait@gate], unsealed.
// The dropoff does not exist yet — that is the whole point.
func gatedStoreReq(id, source, gate string) fleet.CreateOrderRequest {
	return fleet.CreateOrderRequest{
		OrderID: id,
		Blocks: []fleet.OrderBlock{
			{BlockID: id + "-b1", Location: source, BinTask: "JackLoad"},
			{BlockID: id + "-b2", Location: gate, BinTask: "Wait"},
		},
		Complete: false,
	}
}

// gatedTail is what the valve APPENDS to seal the order: [dropoff@slot]. Block
// numbering continues from the create (b3), as stepsToBlocks' blockOffset does.
func gatedTail(id, slot string) []fleet.OrderBlock {
	return []fleet.OrderBlock{{BlockID: id + "-b3", Location: slot, BinTask: "JackUnload"}}
}

// newGatedSim builds the standard lane scene under production physics (mode mouth
// gate + priority hint, no harness deepest-first hold) — the same arm the tiered
// tests measure against, so tick counts are comparable.
func newGatedSim(t *testing.T, slots, hopTicks int) *Sim {
	t.Helper()
	sim := New(wideLaneScene(t, slots), Options{Watchdog: 200, HopTicks: hopTicks})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(true)
	return sim
}

// runPressPair settles a same-origin pair either through the sealed pre-change
// shape or through the valve (create unsealed, append the tail immediately, back
// to back, before any tick — exactly what the scanner pass does when the
// classifier admits). Returns the settle tick count. Fails the test if any robot
// ever dwells in the gated run: an open valve must never park a robot, and that is
// checked EVERY tick so a single-tick dwell cannot hide between samples.
func runPressPair(t *testing.T, hopTicks int, gated bool) int {
	t.Helper()
	specs := []storeSpec{
		{id: "PRESS-L", slot: "S1", approach: 2}, // deep
		{id: "PRESS-R", slot: "S0", approach: 2}, // shallow
	}
	sim := newGatedSim(t, 4, hopTicks)
	for _, sp := range specs {
		if err := sim.AddRobot(sp.id, "AISLE"); err != nil {
			t.Fatalf("AddRobot %s: %v", sp.id, err)
		}
		sim.SetRobotApproach(sp.id, sp.approach)
		if gated {
			if err := sim.Submit(sp.id, gatedStoreReq(sp.id, "LINE", "GATE"), false); err != nil {
				t.Fatalf("Submit %s: %v", sp.id, err)
			}
			if err := sim.AppendBlocks(sp.id, gatedTail(sp.id, sp.slot)); err != nil {
				t.Fatalf("AppendBlocks %s: %v", sp.id, err)
			}
			continue
		}
		if err := sim.Submit(sp.id, storeReq(sp.id, "LINE", sp.slot), false); err != nil {
			t.Fatalf("Submit %s: %v", sp.id, err)
		}
	}
	for range 400 {
		for _, v := range sim.Tick() {
			t.Errorf("press pair (gated=%v) fired a checker: %s: %s", gated, v.Checker, v.Detail)
		}
		if gated {
			if w := sim.WaitingOrders(); len(w) > 0 {
				t.Fatalf("open valve must never dwell; robots waiting at tick %d: %v", sim.TickCount(), w)
			}
		}
		if sim.AllIdle() {
			return sim.TickCount()
		}
	}
	t.Fatalf("press pair (gated=%v) did not settle within 400 ticks", gated)
	return 0
}

// TestGateChoreo_OpenValveIsInvisible is the increment-3 gate: with the lane clear,
// a press pair dispatched through the valve never dwells.
//
// The byte-identity assertion the tiered arm used is RETIRED by the uniform shape —
// a gated store's create genuinely differs from a sealed one (it carries a wait
// block instead of a dropoff), by ruling. Tick-identity replaces it: what must not
// regress is the press pair's time to settle, and the mechanism that guarantees it
// is that the tail is appended before the robot reaches the gate, so the wait is
// satisfied on arrival and costs zero ticks of dwell.
//
// Two things are asserted separately, because they can fail independently:
//   - NO DWELL: WaitingOrders() is empty at every tick. This is the real invariant.
//   - TICK PARITY vs the sealed baseline, reported exactly. Any delta here is
//     travel to the wait point, not gate latency.
func TestGateChoreo_OpenValveIsInvisible(t *testing.T) {
	// The gated run costs exactly two structural extras over the sealed shape, and
	// NEITHER is gate latency:
	//
	//   +hopTicks — one coarse cell-step, because the harness models the wait point
	//               as its own plain node. In the plant it sits on the aisle the
	//               robot already drives through to reach the mouth, so this hop is
	//               a scene-modeling artifact, not travel the robot really adds.
	//   +1        — the sim charges one tick to resolve each block, and the gated
	//               order has three blocks where the sealed one has two.
	//
	// So the honest parity statement is delta == hopTicks+1 EXACTLY. Asserting the
	// exact value at two different hop costs is what proves it: a delta that tracks
	// hopTicks is structural, whereas any real dwell would add ticks that do not
	// move with it. A bare "delta <= something" would have hidden that.
	for _, hop := range []int{3, 5} {
		sealed := runPressPair(t, hop, false)
		gated := runPressPair(t, hop, true)
		delta := gated - sealed
		want := hop + 1
		if delta != want {
			t.Errorf("hopTicks=%d: open valve cost %d ticks over the sealed baseline (%d → %d), want exactly %d (one gate-approach hop + one block resolution). A larger delta is real added latency.",
				hop, delta, sealed, gated, want)
		}
		t.Logf("hopTicks=%d: press pair sealed=%d ticks, through an OPEN valve=%d ticks, delta=%d (= hop %d + 1 block) — zero dwell",
			hop, sealed, gated, delta, hop)
	}
}

// TestGateChoreo_ContendedStagesAndDwells pins increment 3's KNOWN-INCOMPLETE
// behavior as expected, rather than working around it.
//
// When the classifier says the lane is contended, the valve creates the unsealed
// waybill and appends nothing. The robot drives to the wait point and holds —
// indefinitely, because the release evaluator does not exist until increment 4.
// This asserts that shape precisely: the robot dwells, it dwells AT THE GATE and
// not inside the lane, and nothing in the harness mistakes the dwell for a
// deadlock. When increment 4 lands, this test is what proves the dwell became a
// release rather than silently having been a release all along.
func TestGateChoreo_ContendedStagesAndDwells(t *testing.T) {
	sim := newGatedSim(t, 4, 0) // 0 → the sim's default hop cost
	if err := sim.AddRobot("STORE", "AISLE"); err != nil {
		t.Fatalf("AddRobot: %v", err)
	}
	// Created unsealed; the classifier said contended, so NO tail is appended.
	if err := sim.Submit("STORE", gatedStoreReq("store-1", "LINE", "GATE"), false); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	_, vios, settled := sim.RunUntilIdle(300)
	if settled {
		t.Fatal("a contended gated order must NOT complete — nothing has appended its tail")
	}
	for _, v := range vios {
		t.Errorf("staged dwell fired a checker (a dwell is pending, not broken): %s: %s", v.Checker, v.Detail)
	}
	if w := sim.WaitingOrders(); len(w) != 1 || w[0] != "store-1" {
		t.Fatalf("waiting orders = %v, want exactly [store-1] dwelling at the gate", w)
	}
	// It must hold OUTSIDE the lane. A robot that dwelled inside would occupy a
	// lane cell and block the very traffic the gate exists to sequence.
	if r := sim.robots["STORE"]; r.pos.inLane() {
		t.Errorf("gated robot dwelled INSIDE the lane at %v — the wait point must be outside the mouth", r.pos)
	} else if r.pos.Node != "GATE" {
		t.Errorf("gated robot dwelled at %q, want GATE", r.pos.Node)
	}

	// Increment 4 preview, asserted now so the harness proves the dwell is a valve
	// and not a wedge: appending the tail releases it and the store completes.
	if err := sim.AppendBlocks("store-1", gatedTail("store-1", "S1")); err != nil {
		t.Fatalf("AppendBlocks: %v", err)
	}
	ticks, moreVios, done := sim.RunUntilIdle(300)
	if !done {
		t.Fatal("appending the tail must release the dwelling robot and let it finish")
	}
	for _, v := range moreVios {
		t.Errorf("post-release fired a checker: %s: %s", v.Checker, v.Detail)
	}
	if len(sim.WaitingOrders()) != 0 {
		t.Errorf("nothing should still be waiting after the tail landed: %v", sim.WaitingOrders())
	}
	t.Logf("contended store dwelled at the gate, then completed %d ticks after its tail was appended", ticks)
}
