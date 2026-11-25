//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestAdvanceCompound_RefusesASecondDispatchOfOneChild pins EXACTLY-ONCE as a
// property in its own right, separate from the serialization it rode on, which
// is gone (advanceCompound's head).
//
// AdvanceCompoundOrder's sibling-in-flight guard carried two properties fused
// into one condition: "exactly one dispatch per child", which is load-bearing,
// and "one child at a time", which has been removed. The comment that replaced
// the guard says as much — fireCompleted fires on BOTH (*, Delivered) and (Delivered,
// Confirmed), so the function re-enters across one sibling's lifecycle, and the
// createCompound→advanceCompound path used to add a second entry milliseconds
// after creation. The 2026-05-27 incident was one robot's worth of work
// dispatched three times, not three legitimate legs running in parallel.
//
// Remove the loop naively and every child dispatches twice. So exactly-once gets
// its own guard, its own witness, and this test — before anything touches the
// serialization.
//
// THE FIXTURE IS THE CRASH WINDOW. A child that is still `pending` but already
// carries a VendorOrderID is what a crash between the fleet call and the status
// write leaves behind. It was also the only way to reach the guard while the
// sibling loop was in place: construct the state the guard exists for.
func TestAdvanceCompound_RefusesASecondDispatchOfOneChild(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	parent := &orders.Order{
		EdgeUUID: "exactly-once-parent", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: protocol.StatusReshuffling, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	child := &orders.Order{
		EdgeUUID: "exactly-once-child", StationUID: "line-1",
		OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
		ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create child")

	// The fleet already has it: the vendor id was written, the status write was
	// not. Core restarts and the completion event re-enters AdvanceCompoundOrder.
	testutil.MustNoErr(t, db.UpdateOrderVendor(child.ID, "sg-already-out-there", "RUNNING", "AMR-01"),
		"stamp the vendor order id")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("AdvanceCompoundOrder: %v", err)
	}

	reloaded, err := db.GetOrder(child.ID)
	if err != nil {
		t.Fatalf("reload child: %v", err)
	}
	if reloaded.VendorOrderID != "sg-already-out-there" {
		t.Fatalf("child's vendor order id = %q, want the original %q — it was dispatched a SECOND time, "+
			"which is the 2026-05-27 failure: one leg of work, several robots sent to do it",
			reloaded.VendorOrderID, "sg-already-out-there")
	}
}
