//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// mkQueuedComplexParent seeds the row complex intake writes for a buried
// parent, for tests that assert on what the reshuffle tail does with it.
func mkQueuedComplexParent(t *testing.T, db *store.DB, edgeUUID, payloadCode string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:    edgeUUID,
		StationID:   "line-1",
		OrderType:   OrderTypeComplex,
		Status:      StatusQueued,
		Quantity:    1,
		PayloadCode: payloadCode,
		Coordinated: true,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "seed queued complex parent")
	return o
}

// TestBuriedIntake_QueueReasonReachesOrderHistory is the second half, and the
// one that matters after the fact: the row can be corrected later, the history
// cannot.
//
// historyReason copies the order's QueueCode into the history entry for any
// transition into queued. The buried parent's first such transition is the
// reshuffle completing and resuming it. If the column was blank at creation, the
// history entry is blank too, and by then there is nothing left to say what the
// order had been waiting for.
func TestBuriedIntake_QueueReasonReachesOrderHistory(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-QRH-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-QRH-TGT")

	// The SUCCESSFUL disposition, deliberately: the reshuffle plans and
	// dispatches, so none of the three contention arms fires. Those arms have
	// always set a reason; this is the arm that never did, and it is also the
	// common one — an ordinary burial with the lane free.
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := mkQueuedComplexParent(t, db, "uuid-buried-history", bp.Code)
	d.planBuriedReshuffleAtIntake(parent, bp.Code, "line-1",
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	order, err := db.GetOrderByUUID("uuid-buried-history")
	testutil.MustNoErr(t, err, "read back parent")
	if order.Status != StatusReshuffling {
		t.Fatalf("parent status = %q, want %q — the reshuffle did not dispatch, so this test is exercising a contention arm rather than the successful one",
			order.Status, StatusReshuffling)
	}
	if order.QueueCode != string(protocol.QueueStorageRearranging) {
		t.Errorf("queue_code = %q, want %q", order.QueueCode, protocol.QueueStorageRearranging)
	}

	// Drive the transition INTO queued and read what the history recorded. This
	// is the resume the completing reshuffle performs; done directly so the
	// assertion is about historyReason rather than about compound sequencing.
	testutil.MustNoErr(t, d.lifecycle.ResumeCompound(order), "resume the parent back to queued")

	history, err := db.ListOrderHistory(order.ID)
	testutil.MustNoErr(t, err, "list order history")

	var sawQueued bool
	for _, h := range history {
		if h.Status != StatusQueued {
			continue
		}
		sawQueued = true
		if h.Code == "" {
			t.Errorf("order history has a transition into queued with a blank reason code (history id %d).\n"+
				"historyReason copies QueueCode off the row, so a row queued without a reason writes a permanent blank here.", h.ID)
		}
	}
	if !sawQueued {
		t.Fatal("no transition into queued recorded in history — this test asserted nothing")
	}
}
