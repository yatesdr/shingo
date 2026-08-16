//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// compound_admission_docker_test.go — site #4's move, which is a STATED
// BEHAVIOUR CHANGE rather than a characterisation.
//
// AdvanceCompoundOrder asked only "is anyone inside the lane". That is a strict
// subset of "may this move happen now", and per ruling 5 the site does not get
// to keep asking a subset: it either delegates to the whole question or it is
// documented as asking a different one. It is asking the same one, narrowly, so
// it delegates.
//
// The occupancy behaviour itself is characterised by tests that predate this
// move — TestCompound_TwoChildrenInFlightAtOnce and
// TestCompound_LaneGateHoldsWhileOccupied in compound_concurrency_test.go, which
// pin both the overlap the sibling-guard removal was for and the hold that
// replaced it. Those are unchanged and still pass. What is new is below.

// TestCompound_ChildWaitsForADigOnItsDestinationLane is the stated change.
//
// A dig CLAIMS a lane for a whole reshuffle. It does not put anybody inside it,
// so an occupancy-only read sees an empty lane and sends the leg in — to place a
// blocker into a lane another reshuffle owns the mouth of, while that reshuffle
// is between its own moves.
//
// WHY THIS WAS SAFE AND IS ABOUT TO STOP BEING SAFE. Destinations are chosen at
// plan time from ListChildNodesUnlocked, which filters dig-held lanes out in the
// query. So when the whole child set was written at once, every destination had
// been dig-checked moments earlier. The fold is precisely the removal of that
// guarantee: one move is committed at a time, and the lane a later move was
// going to use has had the interval to be claimed by somebody else. This is a
// pre-existing hole that only round 1's shape makes reachable, which is why it
// is fixed here rather than in 5c.
//
// NOT AN AUTOMATIC WIN, per ruling 5 — the check firing where the site works
// today would be §17.3's shape, a reader asking a different question. It is not:
// the dig on the leg's OWN parent still admits (asserted below), so the only
// thing newly refused is a FOREIGN reshuffle's claim on the destination. There
// is no reading under which placing a bin into that lane is correct.
//
// DESIGN §16 rule 7: admission is the first refusal this call can reach. The
// child is pending with both nodes set, both resolve, and nothing occupies
// either lane — the dig is the only thing in the way.
//
// MUTATION (verified): revert the call to the old occupancy-only read. This
// fires — the leg dispatches into a lane another reshuffle holds.
func TestCompound_ChildWaitsForADigOnItsDestinationLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	// The lane this compound digs OUT of, and the lane it places blockers INTO.
	_, srcLaneID, srcSlot := gatedLane(t, db, "CADM-SRC", string(LaneEnforceMouth))
	_, _, dstSlot := gatedLane(t, db, "CADM-DST", string(LaneEnforceMouth))
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cadm-parent"
		o.StationID = "line-1"
		o.Status = protocol.StatusReshuffling
	})
	// The parent owns the source lane's dig — its own legs must still pass.
	if err := reservations.AcquireLanes(db.DB, parent.ID, reservations.ModeDig, "test", srcLaneID); err != nil {
		t.Fatalf("parent dig on the source lane: %v", err)
	}
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cadm-child"
		o.StationID = "line-1"
		o.OrderType = protocol.OrderTypeMove
		o.Status = StatusPending
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
		o.SourceNode = srcSlot.Name
		o.DeliveryNode = dstSlot.Name
	})

	// Baseline: with no foreign claim the leg goes. This is what makes the
	// refusal below attributable to the dig rather than to the fixture.
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (baseline): %v", err)
	}
	if !inFlight(t, db, child.ID) {
		fresh, _ := db.GetOrder(child.ID)
		t.Fatalf("the leg did not dispatch even with both lanes free (status %s) — the fixture never "+
			"reaches the check under test", fresh.Status)
	}

	// The real case, on FULLY SEPARATE lanes. Sharing them with the baseline was
	// the first version and its own guard caught it: the baseline leg took
	// occupancy in the destination lane and never left, so the second compound
	// would have been refused by OCCUPANCY and the test would have passed without
	// the dig check existing. Reusing the source lane had the same defect from the
	// other side — parent2 does not own that dig, so the SOURCE dig would have
	// refused it. Two scenarios, two sets of lanes, one variable.
	_, srcLane2, srcSlot2 := gatedLane(t, db, "CADM-SRC2", string(LaneEnforceMouth))
	_, dstLane2, dstSlot2 := gatedLane(t, db, "CADM-DST2", string(LaneEnforceMouth))

	parent2 := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cadm-parent2"
		o.StationID = "line-1"
		o.Status = protocol.StatusReshuffling
	})
	// parent2 owns its own source dig, exactly as parent did — so the source lane
	// is not what refuses.
	if err := reservations.AcquireLanes(db.DB, parent2.ID, reservations.ModeDig, "test", srcLane2); err != nil {
		t.Fatalf("parent2 dig on its source lane: %v", err)
	}
	// A STRANGER holds the destination lane, and puts nobody inside it.
	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "cadm-stranger" })
	if err := reservations.AcquireLanes(db.DB, stranger.ID, reservations.ModeDig, "test", dstLane2); err != nil {
		t.Fatalf("stranger dig on the destination lane: %v", err)
	}

	child2 := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cadm-child2"
		o.StationID = "line-1"
		o.OrderType = protocol.OrderTypeMove
		o.Status = StatusPending
		o.ParentOrderID = &parent2.ID
		o.Sequence = 1
		o.SourceNode = srcSlot2.Name
		o.DeliveryNode = dstSlot2.Name
	})

	// Both guards: nobody is inside either lane, so occupancy cannot be what
	// answers, and the source dig belongs to this compound.
	for _, g := range []struct {
		name string
		lane int64
	}{{"source", srcLane2}, {"destination", dstLane2}} {
		if occ, _ := reservations.OccupantsOf(db.DB, g.lane); len(occ) != 0 {
			t.Fatalf("%s lane has occupants %v — an occupancy-only read would refuse for the wrong "+
				"reason and this test would pass without the dig check", g.name, occ)
		}
	}

	if err := d.AdvanceCompoundOrder(parent2.ID); err != nil {
		t.Fatalf("advance (dig on destination): %v", err)
	}
	if inFlight(t, db, child2.ID) {
		t.Error("the leg was dispatched into a lane a DIFFERENT reshuffle holds the dig on. Nobody is " +
			"inside that lane, so occupancy alone cannot see this — a dig claims the lane for the " +
			"whole reshuffle, and placing a blocker into it puts a second robot in a corridor one " +
			"order was promised exclusively")
	}
}

// TestCompound_ChildStillPassesItsOwnParentsDig is the exemption, asserted
// separately so the change above cannot be read as "digs now block legs".
//
// This is brief 3 defect 1 restated at the new call site: a leg parked behind
// its own parent's dig deadlocks, because that dig only clears when the leg it
// is parking completes. The baseline arm of the test above exercises it too;
// this states it as its own claim so a future narrowing of the exemption fails
// something that names it.
//
// MUTATION (verified): delete the !d.isOwnDigLeg term in admitLane. This fires,
// and so does the baseline arm above — the deadlock, at the site that would
// suffer it.
func TestCompound_ChildStillPassesItsOwnParentsDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	_, laneID, slot := gatedLane(t, db, "CADM-OWN", string(LaneEnforceMouth))
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cadm-own-parent"
		o.StationID = "line-1"
		o.Status = protocol.StatusReshuffling
	})
	if err := reservations.AcquireLanes(db.DB, parent.ID, reservations.ModeDig, "test", laneID); err != nil {
		t.Fatalf("parent dig: %v", err)
	}
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cadm-own-child"
		o.StationID = "line-1"
		o.OrderType = protocol.OrderTypeMove
		o.Status = StatusPending
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
		o.SourceNode = slot.Name
		o.DeliveryNode = sd.LineNode.Name
	})

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !inFlight(t, db, child.ID) {
		fresh, _ := db.GetOrder(child.ID)
		t.Fatalf("the leg was held behind its OWN parent's dig (status %s). That dig only clears when "+
			"this leg completes, so holding it here is a deadlock — the lock exists to let this work "+
			"run, not to keep it out", fresh.Status)
	}
}
