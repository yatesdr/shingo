//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestC2aPin_ABuriedRetrieveWaitingForParkingHasOneWait — GREEN since C2a A.2
// (red at 660fd228: 10 rows and 10 tells over 5 passes).
//
// THE WAIT FLIP, reproduced on the plain path. A retrieve whose bin is buried,
// in a group whose only parking is in a lane a robot is inside, is asked again on
// every scanner pass. Each pass writes TWO waits:
//
//  1. planBuriedReshuffle's read-failure arm. shuffleSlotsFrom returns
//     LaneOccupiedParkingError ("… is occupied and this dig is 1 slot(s) short"),
//     which is not ErrNoShuffleSlot, and readFailed() is true for any error that
//     is not sql.ErrNoRows, so the planner parks the order itself under
//     waiting_for_slot / read-failed with no params: "Waiting for a slot".
//  2. The scanner's ErrReshuffleWait arm (fulfillment/scanner.go tryFulfill) then
//     writes storage_rearranging with the lane off the wait error:
//     "Rearranging lane <dug> to reach <part>".
//
// The code changes twice per pass, so each pass opens two history rows and tells
// the station twice (orders.WaitOpens / WaitTold). On the packed C1 runs this is
// order 28: 49, 57 and 145 rows in 20 minutes. Nothing failed to read; the wait
// is "no parking yet", one cause, told once.
//
// This drives the planner call and the scanner's park write for PASSES passes, as
// tryFulfill does. The order holds one wait across every pass (C2a A.2: the
// short count comes back as ErrNoShuffleSlot, the planner writes nothing, and
// the scanner parks it under CauseNoShuffleSlot once): one history row, one
// tell.
func TestC2aPin_ABuriedRetrieveWaitingForParkingHasOneWait(t *testing.T) {
	t.Parallel()
	const passes = 5
	db := testDBShared(t)
	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, _, bp := setupDwellGroup(t, db, "C2AF", 1, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "C2AF-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "C2AF-TGT")

	// A robot from another order inside the only lane with room.
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "c2af-inside"; o.Status = StatusDispatched })
	_, aErr := reservations.AcquireOccupancy(db.DB, inside.ID, sib.ID)
	testutil.MustNoErr(t, aErr, "occupy the sibling lane")

	// The waiting retrieve, parked once under the wait it had before the scan.
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.PayloadCode = "c2af-demand", "line-1", OrderTypeRetrieve, bp.Code
		o.Status = StatusQueued
	})
	d.SetQueueReason(o, protocol.QueueWaitingForMaterial, CauseFinderGroupEmpty, QueueParams{Payload: bp.Code})
	buried := &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID}

	rowsBefore := waitRows(t, db, o.ID)
	tellsBefore := len(emitter.waitChanged)
	for pass := 1; pass <= passes; pass++ {
		fresh, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "re-read the order, as tryFulfill does")
		perr := d.PlanBuriedReshuffle(fresh, buried)
		if perr == nil {
			t.Fatalf("pass %d planned the dig: the fixture no longer reaches the park", pass)
		}
		// The scanner's ErrReshuffleWait arm, as written in tryFulfill.
		d.SetQueueReason(fresh, protocol.QueueStorageRearranging, ReshuffleWaitCause(perr),
			ReshuffleWaitParams(perr, fresh.PayloadCode))
	}
	rows := waitRows(t, db, o.ID) - rowsBefore
	tells := len(emitter.waitChanged) - tellsBefore

	if rows != 1 || tells != 1 {
		t.Errorf("a buried retrieve waiting for parking opened %d wait row(s) and told its station %d time(s) "+
			"over %d passes; one wait is one row and one message", rows, tells, passes)
	}
	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "read the order back")
	if QueueCause(after.QueueCause) != CauseNoShuffleSlot {
		t.Errorf("the order waits under %q (%q), want %q: a dig short of parking is short, whatever made it short",
			after.QueueCause, after.QueueReason, CauseNoShuffleSlot)
	}
}

// waitRows counts the order's history rows into a waiting status.
func waitRows(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	var n int
	err := db.DB.QueryRow(`SELECT COUNT(*) FROM order_history WHERE order_id = $1 AND status IN (`+
		protocol.WaitingStatusSQLList()+`)`, orderID).Scan(&n)
	testutil.MustNoErr(t, err, "count wait rows")
	return n
}
