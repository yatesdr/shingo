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

// And the case the bind path was actually built for, which must keep working:
// a carrier is at the slot, the Edge never bound it, and nothing is being held.
// There is no gap to protect, so the correction reconnects it.
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
		Actor:        "admin",
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
