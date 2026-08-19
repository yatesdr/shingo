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
	store := testdb.CreateOrder(t, db)
	admitted, _, _, err = d.AcquireLanesForOrder(store, line, slot, EntryFreshBin)
	testutil.MustNoErr(t, err, "the store's acquire")
	if admitted {
		t.Fatal("a placement into a lane another demand has resolved into must be refused — this is " +
			"the just-moved-then-dug churn, and under the lock it is unconstructible")
	}

	// IT IS A WAIT, NOT A WALL. The lock ends and the store goes in.
	if _, err := reservations.ReleaseLanesByOwner(db.DB, retrieve.ID); err != nil {
		t.Fatalf("release the retrieve's lock: %v", err)
	}
	admitted, _, _, err = d.AcquireLanesForOrder(store, line, slot, EntryFreshBin)
	testutil.MustNoErr(t, err, "the store's second acquire")
	if !admitted {
		t.Fatal("the store was refused after the lock ended — a refusal with no releaser is not a wait")
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

// COMPLEX ACQUIRES. §R.95's census found it never had: admitComplexLanes asked
// admission's physical questions and then took no holds at all, so the whole
// mouth mechanism was reachable only from the plain path — on the traffic class
// that carries both plants.
//
// §R.101's rule is written about complex orders in the owner's own words, so a
// source lock only plain orders take is the rule applied to the smaller half.
//
// MUTATION (verified): delete the acquireOrderLanes call from admitComplexLanes.
// The complex order dispatches into a lane a plain demand has locked, and the
// "must be refused" assertion fires.
func TestResolveLock_ComplexTakesTheSourceLaneToo(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, slot := gatedLane(t, db, "RLOCK-CX", "") // unmarked: the plant's shape
	line := lineNode(t, db, "RLOCK-CX-LINE")

	// A coordinated plan picks out of the lane. Driven through admitComplexLanes
	// itself, not through its parts: the defect was that this FUNCTION took no
	// holds, so a test that calls the derivation and the acquire directly proves
	// they work and says nothing about whether anybody calls them.
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: slot.Name},
		{Action: protocol.ActionDropoff, Node: line.Name},
	}
	// Admission's reachability arm needs the order's bin to resolve, so the plan
	// is a real one: a bin at the shallowest slot, claimed by the order.
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, slot.ID, "RLOCK-CX-BIN")
	cx := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Coordinated = true
		o.SourceNode = slot.Name
		o.DeliveryNode = line.Name
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(cx.ID, bin.ID), "bind the order to its bin")
	testdb.ClaimBinForTest(t, db, bin.ID, cx.ID)
	fresh, err := db.GetOrder(cx.ID)
	testutil.MustNoErr(t, err, "reload the order")
	cx = fresh
	if st := d.admitComplexLanes(cx, steps); st.done {
		t.Fatalf("a free lane must admit the coordinated order: %v", st.err)
	}

	// AND IT NOW OWNS THE LANE. This is the half that was missing: admission's
	// questions were asked and answered, and then nothing was taken, so the lane
	// the order had resolved into was left open behind it.
	if !d.laneLock.IsLocked(laneID) {
		t.Fatal("the coordinated order was admitted and took no hold — complex acquiring nothing is " +
			"§R.95's census result, on the traffic class that carries both plants")
	}

	// A store into the same lane is now refused, which is the churn §R.101 makes
	// unconstructible.
	store := testdb.CreateOrder(t, db)
	admitted, _, _, err := d.AcquireLanesForOrder(store, line, slot, EntryFreshBin)
	testutil.MustNoErr(t, err, "the store's acquire")
	if admitted {
		t.Fatal("a placement into the lane a coordinated order resolved into must be refused")
	}
}

// A plan naming one lane twice — a pickup and a dropoff in the same corridor —
// yields ONE hold, and it is the stronger of the two. Two rows for one owner on
// one lane is the incoherent state admitMouth refuses outright.
func TestResolveLock_OnePlanOneLaneOneHold(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, slot := gatedLane(t, db, "RLOCK-DEDUP", "")
	deeper, err := db.GetNodeByDotName("RLOCK-DEDUP-S1")
	testutil.MustNoErr(t, err, "resolve the deeper slot")

	holds, err := d.resolvePlanLaneHolds([]resolvedStep{
		{Action: protocol.ActionDropoff, Node: slot.Name},
		{Action: protocol.ActionPickup, Node: deeper.Name},
	})
	testutil.MustNoErr(t, err, "resolve")
	if len(holds) != 1 {
		t.Fatalf("holds = %+v, want exactly one for lane %d", holds, laneID)
	}
	if holds[0].mode != reservations.ModeDig {
		t.Fatalf("hold mode = %s, want the stronger of the two — an order that both picks from and "+
			"drops into a lane owns it for the whole visit", holds[0].mode)
	}
}
