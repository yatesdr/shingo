package engine

import (
	"testing"

	"shingo/protocol"
)

// Core now announces every generation change, and the most frequent one by far
// is produce finalize — a press finishes a carrier, Core writes its count and
// starts its next life, and the announcement goes out. That announcement lands
// on a PRODUCE node, which is the one place the Edge is doing something
// delicate at the same instant: the finished carrier is leaving and the next
// one has not arrived.
//
// The two tests below are the two states the slot can be in when it lands.

// The ordinary case: the carrier is still at the slot. The announcement is a
// declaration — a lifecycle event decided this carrier holds this many — so the
// count and the generation are both adopted, and the held pile is not involved.
func TestProduceFinalizeAnnouncement_LandsOnTheCarrierStillAtTheSlot(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "FINALIZE-N1", 9001, 4)

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "FINALIZE-N1",
		NewRemaining: 40,
		Epoch:        5,
		Actor:        "core",
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 5 {
		t.Errorf("epoch = %d, want 5", rt.ActiveBinEpoch)
	}
	if rt.RemainingUOPCached != 40 {
		t.Errorf("remaining = %d, want 40 — a finalize declares the carrier's count", rt.RemainingUOPCached)
	}
	if rt.PendingUOPDelta != 0 {
		t.Errorf("pending = %d, want 0 — nothing was held", rt.PendingUOPDelta)
	}
}

// The delicate case, and the one §9.1 flagged as the highest residual risk of
// the whole change.
//
// The carrier has left, the next has not arrived, and ticks made in that gap
// are being held so they land on the NEXT carrier — a deliberate rule, written
// down, and the reason nothing is lost across a swap. The finalize
// announcement for the DEPARTED carrier arrives in the middle of that.
//
// The Edge has a path that binds a carrier onto an empty slot from an
// adjustment: it exists because a human at Core correcting a count for a
// carrier that was delivered but never bound should reconnect it. That path was
// built for a person acting deliberately. Produce finalize is a machine, firing
// on every press cycle, and it would reach the same path — binding a carrier
// that has physically left, and then the next tick would replay the held pile
// onto it. The parts would be charged to the carrier that has already gone.
//
// So: an announcement carrying a count must not bind a slot that is holding
// ticks for the carrier that has not arrived yet.
func TestProduceFinalizeAnnouncement_DoesNotBindADepartedCarrierOverHeldTicks(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "FINALIZE-N2", 9002, 4)

	// The carrier is picked up: pointer cleared, ticks start piling up.
	if err := eng.db.SetProcessNodeActiveBinID(node, nil); err != nil {
		t.Fatalf("clear the bin pointer on pickup: %v", err)
	}
	if err := eng.db.AddPendingUOPDelta(node, 7); err != nil {
		t.Fatalf("hold ticks during the gap: %v", err)
	}

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID, // the carrier that just left
		CoreNodeName: "FINALIZE-N2",
		NewRemaining: 40,
		Epoch:        5,
		Actor:        "core",
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinID != nil {
		t.Errorf("active bin = %d, want none — the announcement bound a carrier that has "+
			"physically left the slot; the next tick will replay the held pile onto it and "+
			"charge those parts to the wrong carrier", *rt.ActiveBinID)
	}
	if rt.PendingUOPDelta != 7 {
		t.Errorf("pending = %d, want 7 — the held ticks belong to the carrier that has not "+
			"arrived yet and must stay held", rt.PendingUOPDelta)
	}
}

// THE Bound PATH IS DELIBERATELY NOT GUARDED, and this pins why so the next
// reader does not "finish the job" and break a working path.
//
// Round 1 read Bound=true as the same defect: it binds before any guard runs.
// It is not. The two messages make opposite claims. A finalize announcement
// says "this carrier's contents were reset" about a carrier that is on its way
// out; Bound=true says "this carrier is now HERE", and its only two producers
// both have physical evidence for that — an admin Move that Core already
// refused if the destination was occupied, and a transit dropoff firing
// because a bin was physically set down at the node.
//
// The generation announcement never sets Bound (service/bin_manifest.go), so
// the dangerous producer cannot reach this path at all.
//
// It matters that this stays unguarded: the transit dropoff sets NO actor,
// which IsLifecycleActor reads as machine. Applying the rule here would refuse
// a bind that a robot has already made true on the floor, and multi-bin swap
// dropoffs would stop attributing their ticks.
//
// NOT a red-first guard — it characterises behaviour this change leaves alone.
func TestBoundAdjustment_StillBindsAnUnattributedTransitDropoff(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "FINALIZE-N5", 9005, 4)

	if err := eng.db.SetProcessNodeActiveBinID(node, nil); err != nil {
		t.Fatalf("unbind: %v", err)
	}

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "FINALIZE-N5",
		NewRemaining: 40,
		Epoch:        5,
		Bound:        true,
		// wiring_block_completed.go sets no actor on this message.
		Actor: "",
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("active bin = %v, want %d — a transit dropoff physically put this carrier at "+
			"the node; refusing it here would strand the ticks it is about to earn",
			rt.ActiveBinID, binID)
	}
}

// THE ZERO-TICK WINDOW, and it is the one the held-ticks guard above cannot
// see.
//
// Ticks are only held once one has fired. At the instant the robot lifts the
// finished carrier the slot is unbound and pending is still zero — the last
// replay cleared it — and it stays zero until the press completes its next
// part. A finalize announcement is enqueued when the press finishes, crosses
// the outbox and Kafka, and lands inside that interval as a matter of course.
//
// So the guard above is watching for evidence that has not arrived yet, and
// the announcement walks past it into the staged-carrier repair below and
// binds a carrier that is on a robot. What distinguishes the two is not the
// held pile, which is empty in both cases here — it is who is talking.
func TestProduceFinalizeAnnouncement_DoesNotBindADepartedCarrierBeforeTheFirstTick(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "FINALIZE-N4", 9004, 4)

	// The robot has taken the finished carrier. Nothing is held yet: the press
	// has not completed a part since the pointer was cleared.
	if err := eng.db.SetProcessNodeActiveBinID(node, nil); err != nil {
		t.Fatalf("clear the bin pointer on pickup: %v", err)
	}

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID, // the carrier that just left
		CoreNodeName: "FINALIZE-N4",
		NewRemaining: 40,
		Epoch:        5,
		Actor:        protocol.ActorCoreLifecycle,
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinID != nil {
		t.Errorf("active bin = %d, want none — a produce-finalize announcement bound a carrier "+
			"that is on a robot. Every part the press makes from here is charged to it until "+
			"the next carrier physically arrives", *rt.ActiveBinID)
	}
}

// And the case the bind path was actually built for, which must keep working:
// a carrier is at the slot, the Edge never bound it, and nothing is being held.
// There is no gap to protect, so the correction reconnects it.
//
// This is the sibling of the test above and the ONLY thing that separates them
// is the actor. Both arrive at an unbound slot with nothing held, carrying the
// same bin, count and generation. Before provenance was readable the two were
// indistinguishable, and this test pinned the wrong one as correct.
func TestCountCorrection_StillBindsAStagedCarrierWhenNothingIsHeld(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "FINALIZE-N3", 9003, 4)

	if err := eng.db.SetProcessNodeActiveBinID(node, nil); err != nil {
		t.Fatalf("unbind: %v", err)
	}

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "FINALIZE-N3",
		NewRemaining: 40,
		Epoch:        5,
		// A person at the bins page. www.resolveActor substitutes this when a
		// request carries no explicit actor, so it is the real wire value for
		// an operator declaration, not a test-only string.
		Actor: protocol.AuditActorUI,
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("active bin = %v, want %d — the staged-carrier repair still has to work; "+
			"it is the door that reconnects a delivered carrier the Edge never bound",
			rt.ActiveBinID, binID)
	}
	if rt.RemainingUOPCached != 40 {
		t.Errorf("remaining = %d, want 40", rt.RemainingUOPCached)
	}
}
