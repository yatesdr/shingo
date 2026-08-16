//go:build docker

package dispatch

import (
	"strings"
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
	// The SUCCESSFUL arm is "a dig was raised" — no contention fired. Asserting the
	// dig exists is what keeps this test on the successful arm rather than silently
	// drifting onto a contention one.
	//
	// THE STATUS ASSERTION IS INVERTED (§R.91). It read: `want StatusQueued` —
	// "the demand waits with its cause while the service dig runs; a status
	// excursion here means it was re-parented". The demand IS the dig now, so
	// `reshuffling` is the successful arm and `queued` would mean no excavation
	// was taken at all.
	laneClearFor(t, db, order)
	if order.Status != StatusReshuffling {
		t.Fatalf("demand status = %q, want %q — a demand that created a dig becomes its parent and "+
			"wears reshuffling while it runs", order.Status, StatusReshuffling)
	}
	if order.QueueCode != string(protocol.QueueStorageRearranging) {
		t.Errorf("queue_code = %q, want %q", order.QueueCode, protocol.QueueStorageRearranging)
	}

	// THE CAUSE IS ON THE ROW, AND IT STAYS THERE. This is the ordinary burial —
	// no contention arm fired, the dig was raised — and it is exactly the case
	// that used to record nothing.
	if order.QueueCause != string(CauseIntakeBuried) {
		t.Errorf("queue_cause = %q, want %q — an ordinary burial must say why it is waiting, and it "+
			"is the ordinary one that used to record nothing", order.QueueCause, CauseIntakeBuried)
	}
	if strings.TrimSpace(order.QueueReason) == "" {
		t.Error("queue_reason is blank — this is the sentence the operator reads while the dig runs")
	}

	// And it survives the dig being raised, which is the "after the fact" half:
	// the demand is not touched again, so nothing overwrites the cause between the
	// burial and the dispatch.
	again, err := db.GetOrderByUUID("uuid-buried-history")
	testutil.MustNoErr(t, err, "re-read the demand")
	if again.QueueCause != string(CauseIntakeBuried) || strings.TrimSpace(again.QueueReason) == "" {
		t.Errorf("after the dig was raised the demand's cause is (%q, %q) — it must persist for as "+
			"long as the wait does", again.QueueCause, again.QueueReason)
	}
}
