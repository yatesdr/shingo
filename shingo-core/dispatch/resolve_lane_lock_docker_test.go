//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// resolve_lane_lock_docker_test.go — §R.101, the source-lane lock at resolve.
//
// "If a complex order resolves on an open or shallow bin: proceed as normal, lane
// locks, lane clears on the pickup. If a complex order resolves on a buried bin:
// it summons digs, lane lock clears after it achieves the target." One lock
// lifecycle, one owner, start to finish — the owner is always the demand.
//
// The rule is arm 2's dig rule generalized to every order, and what it buys is a
// whole class of churn: a bin cannot be placed in front of a target another
// demand has already resolved onto, so nothing is ever paid to dig out what was
// just put in.

// THE MUTATION THE RULING NAMES: a placement attempted into a resolve-locked lane
// is refused, so the just-moved-then-dug scenario cannot be constructed.
//
// MUTATION (verified): put the source hold back to reservations.ModeOutbound in
// resolveOrderLaneHolds. The store is admitted into the lane the retrieve has
// resolved into, and the "must be refused" assertion fires — which is the churn,
// buildable again.
func TestResolveLock_PlacementIntoALockedLaneIsRefused(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, slot := gatedLane(t, db, "RLOCK-CHURN", "")
	line := lineNode(t, db, "RLOCK-CHURN-LINE")

	// A demand resolves onto a bin in the lane and takes its lane.
	retrieve := testdb.CreateOrder(t, db)
	admitted, _, _, err := d.AcquireLanesForOrder(retrieve, slot, line, EntryFreshBin)
	testutil.MustNoErr(t, err, "the retrieve's acquire")
	if !admitted {
		t.Fatal("a free lane must admit the demand that resolved into it")
	}
	if !d.laneLock.IsLocked(laneID) {
		t.Fatal("resolving onto a bin must LOCK its lane, not queue at its mouth — a doorway hold " +
			"shares, and sharing is what lets a bin land in front of a resolved target")
	}

	// And now the churn: a store tries to put a bin into that same lane. Under a
	// doorway hold this is admitted, the bin lands in front of the target, and a
	// dig is raised to take back out what was just put in.
	// Every lane is marked: the store is sent to the mark and the gate refuses
	// its entry while the lane is locked.
	store := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode, o.DeliveryNode, o.Status = line.Name, slot.Name, StatusPending
	})
	heldAtTheMark(t, d, db, store, line, slot)

	// IT IS A WAIT, NOT A WALL. The lock ends and the store goes in.
	if _, err := reservations.ReleaseLanesByOwner(db.DB, retrieve.ID); err != nil {
		t.Fatalf("release the retrieve's lock: %v", err)
	}
	d.EvaluateLaneReleases(laneID)
	if after, _ := db.GetOrder(store.ID); IsGateStaged(after) {
		t.Fatalf("the store is still held at the mark after the lock ended (cause %q) — a refusal with "+
			"no releaser is not a wait", after.QueueCause)
	}
}

// The OPEN-OR-SHALLOW half of §R.101a: no dig, no legs, and the lock clears on
// the pickup. Nothing else may clear it — in particular not another order's exit
// from the same lane, which is the event that fires the release walk.
//
// MUTATION (verified): drop the holder from the walked population in
// maybeReleaseDigOnLastBlockerOut (walk `open` alone, as it did before §R.101).
// The lock disappears while the demand's bin is still sitting in the lane and the
// "still holds it" assertion fires.
func TestResolveLock_ClearsOnThePickupAndNotBefore(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, slot := gatedLane(t, db, "RLOCK-PICKUP", "")
	line := lineNode(t, db, "RLOCK-PICKUP-LINE")
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, slot.ID, "RLOCK-PICKUP-BIN")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = slot.Name
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusInTransit
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(demand.ID, bin.ID), "bind the demand to its bin")

	admitted, _, _, err := d.AcquireLanesForOrder(demand, slot, line, EntryFreshBin)
	testutil.MustNoErr(t, err, "acquire")
	if !admitted || !d.laneLock.IsLocked(laneID) {
		t.Fatalf("the demand must hold its source lane (admitted=%v locked=%v)", admitted, d.laneLock.IsLocked(laneID))
	}

	// The release walk runs — as it does on every exit event in this lane — while
	// the demand's bin is still sitting there. Before §R.101 the walk looked only
	// at the holder's LEGS, and a demand with no legs looked finished immediately.
	d.maybeReleaseDigOnLastBlockerOut(laneID)
	if !d.laneLock.IsLocked(laneID) {
		t.Fatal("the lock was dropped while the demand's bin was still in the lane — the holder is " +
			"in the population, not just its legs; an open resolve has no legs at all")
	}

	// THE PICKUP. The bin leaves the lane by its mover, which is the whole of the
	// release predicate, and the corridor opens.
	testutil.MustNoErr(t, db.MoveBinToTransit(bin.ID, transitNode(t, db, "RLOCK-PICKUP-TRANSIT").ID),
		"the demand picks its bin up")
	d.maybeReleaseDigOnLastBlockerOut(laneID)
	if d.laneLock.IsLocked(laneID) {
		t.Fatal("the lock outlived the pickup — the lane frees when the target leaves by its mover")
	}
}
