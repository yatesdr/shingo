package orders

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	storeorders "shingoedge/store/orders"
)

// TestCancelledRowDropsItsQueueText pins that a row which goes terminal no
// longer reads as waiting. A queued row carries Core's reason for the wait; once
// the order is cancelled that sentence ("Waiting for a slot at ...") is false,
// so the status write clears it along with its code.
func TestCancelledRowDropsItsQueueText(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, err := db.CreateOrder("uuid-q-cancel", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(oid, string(StatusQueued)), "queue it")
	testutil.MustNoErr(t, db.SetOrderQueueReason("uuid-q-cancel", "Waiting for a slot at SYN-LANE-1", "waiting_for_slot"), "set reason")

	testutil.MustNoErr(t, mgr.AbortOrderWithReason(oid, "operator cancel"), "cancel")
	o, err := db.GetOrder(oid)
	testutil.MustNoErr(t, err, "reload")
	if o.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", o.Status)
	}
	if o.QueueReason != "" || o.QueueCode != "" {
		t.Errorf("cancelled row still reads queue_reason=%q queue_code=%q, want both blank", o.QueueReason, o.QueueCode)
	}
}

// TestNonTerminalStatusKeepsQueueText pins the other side: a status write that
// is not terminal leaves Core's reason where it is. The reason is owned by the
// queue-reason write, and a sourcing/queued bounce must not erase it.
func TestNonTerminalStatusKeepsQueueText(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)

	oid, err := db.CreateOrder("uuid-q-keep", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(oid, string(StatusQueued)), "queue it")
	testutil.MustNoErr(t, db.SetOrderQueueReason("uuid-q-keep", "Waiting for a slot at SYN-LANE-1", "waiting_for_slot"), "set reason")
	testutil.MustNoErr(t, db.UpdateOrderStatus(oid, string(StatusSourcing)), "re-source")

	o, err := db.GetOrder(oid)
	testutil.MustNoErr(t, err, "reload")
	if o.QueueReason == "" || o.QueueCode == "" {
		t.Errorf("non-terminal write cleared queue text (reason=%q code=%q)", o.QueueReason, o.QueueCode)
	}
}

// TestTerminalProjectionCarriesNoQueueText pins the projection upsert, the
// other statement that writes an order's status: a terminal projection lands
// with blank queue text even when the projection carries a reason.
func TestTerminalProjectionCarriesNoQueueText(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)

	oid, err := db.CreateOrder("uuid-q-proj", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.SetOrderQueueReason("uuid-q-proj", "Waiting for a slot at SYN-LANE-1", "waiting_for_slot"), "set reason")

	_, err = storeorders.UpsertProjection(db.DB, storeorders.ProjectionRow{
		UUID: "uuid-q-proj", OrderType: protocol.OrderTypeRetrieve, Status: string(protocol.StatusCancelled),
		DeliveryNode: "X", QueueReason: "Waiting for a slot at SYN-LANE-1", QueueCode: "waiting_for_slot",
	})
	testutil.MustNoErr(t, err, "project terminal")
	o, err := db.GetOrder(oid)
	testutil.MustNoErr(t, err, "reload")
	if o.QueueReason != "" || o.QueueCode != "" {
		t.Errorf("terminal projection left queue_reason=%q queue_code=%q, want both blank", o.QueueReason, o.QueueCode)
	}
}
