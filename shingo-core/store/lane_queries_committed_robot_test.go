//go:build docker

package store_test

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestBurialGuard_APlanNeverBlocksACommittedRobot: the demo jam of 2026-10-08
// (c2b-demo-after-b267fc26-20m, Lane_04), in order:
//
//  1. 21:44:58.6  order 765 (complex PANEL-B -> SYN_MARKET) arrives. Its
//     pickup, bin 24, stands at the back of Lane_04 behind two blockers.
//  2. 21:45:02    765's dig starts (children 766, 767 lift the blockers) and
//     soft-reserves bin 24 at its birth (A.1b), so the slots it empties are
//     not refilled in front of the bin before 765's next pass.
//  3. 21:45:10.6  order 779 (a press bin, PLN_001) is dispatched gated into
//     Lane_04 with slot SMN_011, in front of bin 24, confirmed in its hand:
//     8 s into 765's dig, while 767 still dwells in the lane.
//  4. 21:45:15.8  779's robot reaches the waiting spot and the release asks
//     the store selector, whose burial clause reads 765's pending hold as
//     "coming for" bin 24: "lane closed to stores", refused 70 times.
//  5. 21:45:16    765's dig ends and its own drop finds no slot in SYN_MARKET,
//     whose free slots are the ones held by robots at the marks, 779 among
//     them. 765 waits on 779, 779 on 765, until the run ends 50 minutes on.
//
// A plan never blocks a committed robot. 765 has not been sent: its hold is a
// plan, re-made on its next pass, and a burial it meets becomes a dig the
// burial instrument counts. 779 is with the fleet and holds a confirmed slot in
// the lane: it enters. A store that has not been sent is still refused, which
// is the naked-target window A.1b closed.
func TestBurialGuard_APlanNeverBlocksACommittedRobot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, slot2ID, slot3ID := laneFixture(t, db, "BG-COMMIT")

	target := testdb.CreateBinAtNode(t, db, "BIN-BG-COMMIT", slot3ID, "BIN-BG-COMMIT")
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status, o.QueueCode = "complex", protocol.StatusQueued, string(protocol.QueueWaitingForSlot)
	})
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status, o.ParentOrderID = "move", protocol.StatusConfirmed, &demand.ID
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, demand.ID, demand.ID, target.ID, "dig birth"), "the dig's soft hold")

	committed := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status, o.VendorOrderID = "complex", protocol.StatusStaged, "sg-commit-1"
	})
	testutil.MustNoErr(t, reservations.AcquireSlot(db.DB, committed.ID, slot2ID, "hand"), "the robot's slot")
	testutil.MustNoErr(t, reservations.ConfirmSlot(db.DB, committed.ID, slot2ID), "confirmed in its hand")

	got, err := db.FindStoreSlotInLaneExcluding(laneID, committed.ID)
	if err != nil {
		t.Fatalf("release of the committed robot: %v — a never-sent demand's pending hold closed the lane "+
			"to a robot already holding a confirmed slot in it", err)
	}
	if got.ID != slot2ID {
		t.Fatalf("store slot = %s, want its own confirmed slot %d", got.Name, slot2ID)
	}

	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status = "store", protocol.StatusQueued
	})
	if got, err := db.FindStoreSlotInLaneExcluding(laneID, stranger.ID); err == nil && got.ID != slot2ID {
		t.Fatalf("an unsent store was offered %s in front of the dug-for bin — the naked-target window is open again", got.Name)
	} else if err != nil && !errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Fatalf("an unsent store's refusal = %v, want ErrLaneClosedByClaim", err)
	}
}

// TestBurialGuard_AnUnsentOrdersAimIsAPlan is the aim's half of the same
// principle. A never-sent order resolved onto a bin (bin_id and source_node on
// the bin's slot) is a plan like a pending hold: it re-makes on its next pass,
// so it does not close the lane to a robot holding a confirmed slot there. A
// holder the fleet has is moving, and its aim still closes the lane to
// everyone. Choosing a slot still respects every plan: an unsent store is
// refused in front of the aimed bin.
//
// MUTATION: drop "AND NOT (AwaitingFleet)" from ComingForBinCommittedSQL's aim
// arm; the committed robot is refused by the unsent aim.
func TestBurialGuard_AnUnsentOrdersAimIsAPlan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, _, slot2ID, slot3ID := laneFixture(t, db, "BG-AIM")
	slot3, err := db.GetNode(slot3ID)
	testutil.MustNoErr(t, err, "read the target slot")

	target := testdb.CreateBinAtNode(t, db, "BIN-BG-AIM", slot3ID, "BIN-BG-AIM")
	aimer := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status = "retrieve", protocol.StatusQueued
		o.BinID, o.SourceNode = &target.ID, slot3.Name
	})

	committed := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status, o.VendorOrderID = "complex", protocol.StatusStaged, "sg-aim-1"
	})
	testutil.MustNoErr(t, reservations.AcquireSlot(db.DB, committed.ID, slot2ID, "hand"), "the robot's slot")
	testutil.MustNoErr(t, reservations.ConfirmSlot(db.DB, committed.ID, slot2ID), "confirmed in its hand")

	got, err := db.FindStoreSlotInLaneExcluding(laneID, committed.ID)
	if err != nil {
		t.Fatalf("release of the committed robot: %v — a never-sent order's aim closed the lane to a robot "+
			"already holding a confirmed slot in it", err)
	}
	if got.ID != slot2ID {
		t.Fatalf("store slot = %s, want its own confirmed slot %d", got.Name, slot2ID)
	}

	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status = "store", protocol.StatusQueued
	})
	if got, err := db.FindStoreSlotInLaneExcluding(laneID, stranger.ID); err == nil && got.ID != slot2ID {
		t.Fatalf("an unsent store was offered %s in front of an aimed bin — choosing a slot must respect every plan", got.Name)
	} else if err != nil && !errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Fatalf("an unsent store's refusal = %v, want ErrLaneClosedByClaim", err)
	}

	// The aimer is sent, claiming its bin in its hand: now it is moving, and the
	// lane is closed to the committed robot too.
	testutil.MustNoErr(t, reservations.Acquire(db.DB, aimer.ID, aimer.ID, target.ID, "hand"), "the aimer reserves its bin")
	testutil.MustNoErr(t, db.ClaimBin(target.ID, aimer.ID), "the aimer's hand claims its bin")
	testutil.MustNoErr(t, db.UpdateOrderVendor(aimer.ID, "sg-aim-0", "RUNNING", "AMR-1"), "send the aimer")
	_, err = db.Exec(`UPDATE orders SET status=$1 WHERE id=$2`, protocol.StatusDispatched, aimer.ID)
	testutil.MustNoErr(t, err, "mark the aimer dispatched")
	if got, err := db.FindStoreSlotInLaneExcluding(laneID, committed.ID); err == nil {
		t.Fatalf("the committed robot was offered %s in front of a bin a sent robot is coming for", got.Name)
	} else if !errors.Is(err, nodes.ErrLaneClosedByClaim) {
		t.Fatalf("the committed robot's refusal = %v, want ErrLaneClosedByClaim", err)
	}
}
