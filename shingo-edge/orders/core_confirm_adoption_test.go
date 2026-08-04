package orders

import (
	"testing"

	"shingo/protocol"
)

// Core-side completion adoption — ApplyCoreStatus's StatusConfirmed arm.
//
// Confirmation is the one terminal fact the fleet never reports (it is
// paperwork, not movement), so it cannot ride the fleet arm the way faulted and
// in_transit do, and it has no dedicated envelope the way staged and delivered
// do. Before this arm existed, a completion Core decided ON ITS OWN reached the
// Edge through nothing at all — the row sat at `delivered` until the next
// restart, and a stranded `delivered` row is not inert: it stays in the node's
// active-order list (delivered is non-terminal), where the operator-station
// modal reads it as a live leg.
//
// Springfield ALN_001, 2026-08-03: order 4017 was confirmed by Core's
// stuck-delivered sweep at 20:39 and still read `delivered` on the Pi at 22:39,
// by which point it had captured the "blocker" slot in a LATER changeover's
// modal and was displaying a queue_reason naming the superseded style.

func TestApplyCoreStatus_AdoptsCoreConfirm(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-core-confirm", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusDelivered))

	order, _ := db.GetOrder(oid)
	if err := mgr.ApplyCoreStatus(order, protocol.StatusConfirmed, "confirmed at Core"); err != nil {
		t.Fatalf("ApplyCoreStatus: %v", err)
	}

	got, _ := db.GetOrder(oid)
	if got.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q — a Core-side confirm must land on the Edge row, "+
			"or it lingers in the node's active list as a phantom leg", got.Status, StatusConfirmed)
	}
}

// The echo. The normal direction is Edge confirms → tells Core → Core completes
// → Core now announces that completion straight back. The announcement must cost
// nothing: no second status write, no duplicate history row, and above all no
// receipt filed back to Core, which would be a receipt for a receipt.
func TestApplyCoreStatus_CoreConfirmEchoIsNoop(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-echo", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusDelivered))
	_ = db.UpdateOrderStatus(oid, string(StatusConfirmed))

	before, _ := db.ListOrderHistory(oid)

	order, _ := db.GetOrder(oid)
	if err := mgr.ApplyCoreStatus(order, protocol.StatusConfirmed, "confirmed at Core"); err != nil {
		t.Fatalf("ApplyCoreStatus: %v", err)
	}

	after, _ := db.ListOrderHistory(oid)
	if len(after) != len(before) {
		t.Errorf("history grew from %d to %d rows — the echo must not re-write a row "+
			"that is already terminal", len(before), len(after))
	}
}

// A Core confirm must not resurrect an order the Edge already put to rest for a
// DIFFERENT reason. Cancelled is terminal; a late completion announcement
// arriving after a cancel would otherwise flip a dead order back to confirmed
// and put it back on the board.
func TestApplyCoreStatus_CoreConfirmDoesNotResurrectTerminal(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-cancelled", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusCancelled))

	order, _ := db.GetOrder(oid)
	if err := mgr.ApplyCoreStatus(order, protocol.StatusConfirmed, "confirmed at Core"); err != nil {
		t.Fatalf("ApplyCoreStatus: %v", err)
	}

	got, _ := db.GetOrder(oid)
	if got.Status != StatusCancelled {
		t.Errorf("status = %q, want %q — a completion announcement must never "+
			"resurrect an order that reached a different terminal state", got.Status, StatusCancelled)
	}
}
