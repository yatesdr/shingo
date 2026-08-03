package scenesim

import (
	"slices"
	"testing"

	"shingocore/fleet"
)

// buried_retrieve_test.go — INCREMENT 6: buried-retrieve pre-positioning.
//
// THE CLAIM UNDER TEST, stated so a failure means something. A retrieve whose bin
// is buried cannot enter the lane while a dig holds it. Today it waits in Core
// with no robot committed, so its drive to the lane happens AFTER the dig
// finishes. The increment sends it out immediately as an unsealed waybill that
// dwells at the lane's gate point, so the drive overlaps the dig and the robot is
// already standing there when the lane opens.
//
// WHAT THIS DOES NOT CLAIM, and the harness must not accidentally prove: entry at
// exposure. The bin is physically reachable as soon as the last blocker is lifted,
// but the dig's ModeDig mouth row excludes all comers until the dig COMPLETES, and
// v1 does not touch that. Releasing the dig row early is its own increment and its
// own owner decision. So the retrieve here is released on dig COMPLETION, and the
// gain is travel overlap only.
//
// The bound on that gain is structural and worth stating: robots cannot pass each
// other in a lane, so nothing about the in-lane work parallelizes. Only the
// approach does. A scenario that showed a large win would mean the model had
// stopped respecting single-file physics.
//
// ────────────────────────────────────────────────────────────────────────────
// THESE SCENARIOS DO NOT RUN YET, AND THE REASON IS NOT ABOUT THIS INCREMENT.
//
// scenesim has no OUTBOUND physics. Every scenario in this package — the S1
// wounds, S2, tiered entry, gate choreography, mode share, the wall experiments —
// is a STORE going INTO a lane. Measured 2026-07-31: a lone robot, no gate, no
// dig, nothing in its way, asked to pick a bin out of a lane slot and carry it to
// the line, never moves. The bin is still in its slot 200 ticks later and the
// no-deadlock checker fires.
//
// A buried retrieve is therefore not expressible in the harness, which means
// increment 6 cannot be sim-gated the way SYNTH-round2 requires every increment to
// be. Building the harness's outbound/digger physics is README item 10 ("Digger
// flows (sim-only) + harness digger checks"), and it is a hard PREREQUISITE for
// increment 6 rather than the parallel nice-to-have the plan currently implies.
//
// So this file is the SPEC, parked. It states what increment 6 has to prove, and
// requireOutboundPhysics below is SELF-CLEARING: it re-runs that measurement on
// every test run and these scenarios begin executing the day outbound physics
// lands. Nobody has to remember to un-skip them.
// ────────────────────────────────────────────────────────────────────────────

// requireOutboundPhysics skips the caller unless the harness can model the
// simplest possible retrieve: one robot, one bin, no gate, no dig, no contention.
//
// Deliberately a live probe rather than a hardcoded skip. A t.Skip with a comment
// rots — it keeps skipping long after the reason is gone, and nothing tells you.
// This asks the harness the question every run, so the gate opens by itself.
func requireOutboundPhysics(t *testing.T) {
	t.Helper()
	sim := New(wideLaneScene(t, 3), Options{Watchdog: 200})
	sim.PlaceBin("S0")
	if err := sim.AddRobot("PROBE", "AISLE"); err != nil {
		t.Fatalf("probe AddRobot: %v", err)
	}
	if err := sim.Submit("PROBE", storeReq("probe", "S0", "LINE"), false); err != nil {
		t.Fatalf("probe Submit: %v", err)
	}
	if _, _, settled := sim.RunUntilIdle(200); !settled {
		t.Skip("scenesim has no outbound physics — a lone robot cannot pick a bin out of a " +
			"lane slot and carry it away, so a buried retrieve is not expressible. That is " +
			"README item 10 (digger flows, sim-only), and it gates increment 6. This guard " +
			"is self-clearing: these scenarios run as soon as the probe settles.")
	}
}

// gatedRetrieveReq is the buried-retrieve create: a dwell at the lane's gate point
// and NOTHING ELSE.
//
// This is the shape difference from a gated STORE, and it is the reason the
// increment buys less than the store case does. A store's create carries
// [pickup@source, wait@gate] — its pickup is outside the lane, so the robot does
// real work before it dwells. A retrieve's pickup IS the lane slot, so there is no
// legal work available before the lane opens. Every block that does anything
// arrives in the appended tail.
func gatedRetrieveReq(id, gate string) fleet.CreateOrderRequest {
	return fleet.CreateOrderRequest{
		OrderID:  id,
		Blocks:   []fleet.OrderBlock{{BlockID: id + "-w", Location: gate, BinTask: "Wait"}},
		Complete: false,
	}
}

// retrieveTail is what Core appends when the dig's mouth row clears.
//
// THE SLOT IS AN ARGUMENT BECAUSE IT IS RESOLVED HERE, at append time. A dig moves
// bins — in target-node mode it can relocate the wanted bin entirely — so the slot
// the order was born wanting is not necessarily the slot the bin occupies when the
// lane finally opens. Production must re-resolve at release for the same reason
// rebindGatedDropoff re-resolves a store's destination; binding this at create
// time would be the same defect pointed the other way.
func retrieveTail(id, slot, dest string) []fleet.OrderBlock {
	return []fleet.OrderBlock{
		{BlockID: id + "-t1", Location: slot, BinTask: "JackLoad"},
		{BlockID: id + "-t2", Location: dest, BinTask: "JackUnload"},
	}
}

// digReq is a one-leg dig: lift the shallow blocker out of the lane and carry it
// away. Submitted with dig=true so it holds the lane in dig mode, which is what
// the mode-purity checker enforces exclusion against.
func digReq(id, slot, dest string) fleet.CreateOrderRequest {
	return storeReq(id, slot, dest)
}

// TestBuriedRetrieve_PrePositionsDuringDig is the behavioural assertion: the
// retrieve's robot is standing at the gate point WHILE the dig still owns the
// lane, and does not enter until the dig is done.
//
// Both halves matter. Dwelling during the dig is the whole gain. Not entering
// during the dig is the safety property the mode checker would otherwise catch as
// a violation — asserted explicitly as well, so a future model change that
// silently allowed co-occupancy fails here with a sentence rather than as a
// checker firing somewhere downstream.
func TestBuriedRetrieve_PrePositionsDuringDig(t *testing.T) {
	requireOutboundPhysics(t)
	sim := newGatedSim(t, 3, 0)
	sim.PlaceBin("S0") // the blocker, shallow — what the dig lifts
	sim.PlaceBin("S1") // the bin actually wanted, behind it

	if err := sim.AddRobot("DIGGER", "AISLE"); err != nil {
		t.Fatalf("AddRobot DIGGER: %v", err)
	}
	if err := sim.AddRobot("RETRIEVER", "AISLE"); err != nil {
		t.Fatalf("AddRobot RETRIEVER: %v", err)
	}
	// Parked far enough that "did the drive overlap the dig" has a visible answer,
	// but NOT so far the retriever arrives after the dig already cleared (then there
	// is no dwell window by construction and the pre-positioning is invisible). A
	// one-bin dig settles in ~9 ticks; at the default HopTicks=3, approach=1 cell is
	// ~3 ticks of travel, landing the retriever at the gate at ~tick 7 — mid-dig,
	// with a 2-tick dwell before the lane opens.
	sim.SetRobotApproach("RETRIEVER", 1)

	if err := sim.Submit("DIGGER", digReq("dig-1", "S0", "LINE"), true); err != nil {
		t.Fatalf("Submit dig: %v", err)
	}
	// The increment: the retrieve goes out NOW, unsealed, rather than waiting in
	// Core for the dig to finish.
	if err := sim.Submit("RETRIEVER", gatedRetrieveReq("buried-1", "GATE"), false); err != nil {
		t.Fatalf("Submit staged retrieve: %v", err)
	}

	var (
		dwelledDuringDig bool
		appended         bool
		appendTick       int
		settleTick       int
	)
	for range 400 {
		for _, v := range sim.Tick() {
			t.Errorf("checker fired at tick %d: %s: %s", sim.TickCount(), v.Checker, v.Detail)
		}
		digActive := sim.OrderActive("dig-1")
		if digActive && slices.Contains(sim.WaitingOrders(), "buried-1") {
			dwelledDuringDig = true
		}
		// Core's release: the dig is done, its mouth row is gone, append the tail.
		if !digActive && !appended {
			if err := sim.AppendBlocks("buried-1", retrieveTail("buried-1", "S1", "LINE")); err != nil {
				t.Fatalf("AppendBlocks at tick %d: %v", sim.TickCount(), err)
			}
			sim.ReleaseWait("buried-1")
			appended, appendTick = true, sim.TickCount()
		}
		if appended && sim.AllIdle() {
			settleTick = sim.TickCount()
			break
		}
	}

	if !appended {
		t.Fatal("the dig never completed — the scenario asserted nothing")
	}
	if settleTick == 0 {
		t.Fatalf("never settled (waiting=%v)", sim.WaitingOrders())
	}
	if !dwelledDuringDig {
		t.Error("the retrieve never dwelled at the gate while the dig held the lane — " +
			"it was not pre-positioned, which is the entire increment")
	}
	if sim.HasBin("S1") {
		t.Error("the wanted bin is still in its slot — the retrieve never actually picked it up")
	}
	t.Logf("buried retrieve: dig cleared at tick %d, settled at %d (robot was already at the gate)",
		appendTick, settleTick)
}

// TestBuriedRetrieve_PrePositioningBeatsSerialDispatch is the VALUE claim, and it
// is the one that can honestly fail.
//
// Baseline is today's behaviour: the retrieve is not dispatched at all until the
// dig completes, so its full approach is serial with the dig. Gated is the
// increment. The measure is total settle ticks from one clock, and the difference
// should be the approach that got overlapped — no more, because in-lane work
// cannot parallelize.
//
// If this ever reports parity, the increment is not worth its complexity on this
// geometry and that is a real finding, not a broken test.
func TestBuriedRetrieve_PrePositioningBeatsSerialDispatch(t *testing.T) {
	requireOutboundPhysics(t)
	// approach is in COARSE CELLS (each costs Options.HopTicks ticks), and it must
	// fit inside the one-bin dig window (~9 ticks) or there is no overlap to measure.
	// 2 cells ≈ 6 ticks of travel: enough to overlap the dig meaningfully, small
	// enough that the retriever still reaches the gate before the lane opens.
	const approach = 2
	const hopTicks = 3 // matches newGatedSim's Options.HopTicks
	approachTicks := approach * hopTicks

	serial := runBuriedRetrieve(t, approach, false)
	gated := runBuriedRetrieve(t, approach, true)

	if serial == 0 || gated == 0 {
		t.Fatalf("a run failed to settle (serial=%d gated=%d)", serial, gated)
	}
	if gated > serial {
		t.Errorf("pre-positioning made it SLOWER: gated %d ticks vs serial %d — "+
			"the staged robot is getting in its own way", gated, serial)
	}
	saved := serial - gated
	if saved == 0 {
		t.Errorf("pre-positioning bought nothing (both %d ticks) — on this geometry the "+
			"increment does not pay for itself; re-read before building it", serial)
	}
	if saved > approachTicks {
		t.Errorf("saved %d ticks against a %d-cell approach (%d ticks) — more than the "+
			"overlap can physically account for, so the model is letting the retrieve "+
			"into the lane early and is no longer testing v1", saved, approach, approachTicks)
	}
	t.Logf("BURIED RETRIEVE pre-positioning: serial %d ticks, gated %d, saved %d of a %d-tick (%d-cell) approach",
		serial, gated, saved, approachTicks, approach)
}

// runBuriedRetrieve settles one dig + one buried retrieve and returns the tick it
// settled on. gated=false is today's shape (dispatch the retrieve only once the
// dig is done); gated=true is the increment (dispatch it unsealed up front).
func runBuriedRetrieve(t *testing.T, approach int, gated bool) int {
	t.Helper()
	sim := newGatedSim(t, 3, 0)
	sim.PlaceBin("S0")
	sim.PlaceBin("S1")
	if err := sim.AddRobot("DIGGER", "AISLE"); err != nil {
		t.Fatalf("AddRobot DIGGER: %v", err)
	}
	if err := sim.AddRobot("RETRIEVER", "AISLE"); err != nil {
		t.Fatalf("AddRobot RETRIEVER: %v", err)
	}
	sim.SetRobotApproach("RETRIEVER", approach)

	if err := sim.Submit("DIGGER", digReq("dig-1", "S0", "LINE"), true); err != nil {
		t.Fatalf("Submit dig: %v", err)
	}
	if gated {
		if err := sim.Submit("RETRIEVER", gatedRetrieveReq("buried-1", "GATE"), false); err != nil {
			t.Fatalf("Submit staged retrieve: %v", err)
		}
	}

	released := false
	for range 400 {
		for _, v := range sim.Tick() {
			t.Errorf("checker fired at tick %d (gated=%v): %s: %s", sim.TickCount(), gated, v.Checker, v.Detail)
		}
		if !sim.OrderActive("dig-1") && !released {
			if gated {
				if err := sim.AppendBlocks("buried-1", retrieveTail("buried-1", "S1", "LINE")); err != nil {
					t.Fatalf("AppendBlocks: %v", err)
				}
				sim.ReleaseWait("buried-1")
			} else {
				// Today: the order is only created once the lane is free, so the
				// robot starts its approach from cold.
				if err := sim.Submit("RETRIEVER", storeReq("buried-1", "S1", "LINE"), false); err != nil {
					t.Fatalf("Submit serial retrieve: %v", err)
				}
			}
			released = true
		}
		if released && sim.AllIdle() {
			return sim.TickCount()
		}
	}
	return 0
}
