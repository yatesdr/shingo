//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// --- TC-38: Cancel delivered order must not create return order / receipt on cancelled order ---
// Scenario (reproduces production incident from 2026-03-30):
//  1. Retrieve order dispatched, robot runs and finishes → status = "delivered"
//  2. Admin cancels the order (before operator confirms receipt) → status = "cancelled"
//  3. Operator confirms receipt on the already-cancelled order → status = "confirmed"
//
// Two bugs exposed:
//
//	Bug A: maybeCreateReturnOrder fires on the cancelled event and creates a return order
//	        that claims the bin, even though the bin was already delivered to lineside.
//	        The return order has SourceNode = warehouse (wrong — bin is physically at lineside).
//	Bug B: ConfirmReceipt does not guard against cancelled orders. It overwrites status
//	        from "cancelled" back to "confirmed" and calls ApplyBinArrival, moving the bin
//	        in the DB to lineside while it's claimed by the return order.
//
// Result: bin is at lineside in DB, claimed by a return order that thinks it's at the
// warehouse. Return order can't dispatch. Bin is permanently locked. Team can't release
// lineside or run new orders against that bin.
func TestCancelDeliveredOrder_NoReturnCreated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC38")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Create retrieve order — bin at storage, robot dispatched.
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "tc38-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrderStatus(t, db, "tc38-1", dispatch.StatusDispatched)
	binID := *order.BinID

	// Step 2: Robot runs and finishes. Order status = "delivered".
	// Bin is physically at lineside, but DB still has it at source (ApplyBinArrival
	// only fires on receipt confirmation, which hasn't happened yet).
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "tc38-1", "delivered")
	t.Logf("after fleet FINISHED: order=%d status=delivered bin=%d", order.ID, binID)

	// Step 3: Admin cancels the delivered order (3:21 PM in production incident).
	// BUG A: maybeCreateReturnOrder fires and creates a spurious return order.
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "tc38-1",
		Reason:    "cancelled by admin",
	})

	order = testdb.RequireOrderStatus(t, db, "tc38-1", dispatch.StatusCancelled)

	// Verify no spurious auto_return order was created.
	t.Logf("checking for spurious return orders after cancel of delivered order %d", order.ID)
	returnOrders, err := db.ListOrdersByBin(binID, 10)
	if err != nil {
		t.Fatalf("list orders for bin %d: %v", binID, err)
	}
	for _, o := range returnOrders {
		if o.PayloadDesc == "auto_return" {
			t.Errorf("BUG A: auto_return order %d created for delivered order %d — "+
				"delivered orders must not generate return orders (bin is at lineside)",
				o.ID, order.ID)
		}
	}

	// Verify bin is unclaimed after cancel (no return order holding it).
	testdb.AssertBinUnclaimed(t, db, binID)

	// Step 4: Operator confirms receipt on the now-cancelled order (3:36 PM in production).
	// BUG B: ConfirmReceipt does not reject receipts on cancelled orders.
	// It overwrites status from "cancelled" to "confirmed" and runs ApplyBinArrival.
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "tc38-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order, _ = db.GetOrderByUUID("tc38-1")
	t.Logf("after receipt on cancelled order: status=%s (should still be cancelled)", order.Status)

	if order.Status == dispatch.StatusConfirmed {
		t.Errorf("BUG B: receipt on cancelled order %d changed status to %q — "+
			"ConfirmReceipt must reject cancelled orders", order.ID, order.Status)
	}

	// Final state: bin should be at lineside, unclaimed, and usable.
	testdb.AssertBinUnclaimed(t, db, binID)
	bin := testdb.RequireBin(t, db, binID)
	t.Logf("final: bin=%d node=%v claimed=%v", binID, bin.NodeID, bin.ClaimedBy)
}

// --- TC-39: TerminateOrder rejects terminal statuses ---
// Scenario: Admin clicks Terminate on a confirmed/completed order. The API must reject
// this because the bin is already at its destination and the order is final.
//
// Bug: TerminateOrder (orders.go:76) has no status guard. It unclaims bins, overwrites
// status to "cancelled", and emits EventOrderCancelled — which triggers
// maybeCreateReturnOrder. The return order claims the bin with the wrong source node,
// permanently locking it at lineside.
//
// This is the root enabler for the TC-38 incident. If TerminateOrder rejected terminal
// statuses, the spurious return order and receipt-on-cancelled bugs wouldn't trigger.
func TestTerminateOrder_RejectsConfirmedStatus_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC39")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()
	var err error

	// Full lifecycle: dispatch → run → finish → confirm receipt
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "tc39-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "tc39-1")
	binID := *order.BinID

	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "tc39-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "tc39-1", dispatch.StatusConfirmed)

	bin, _ := db.GetBin(binID)
	t.Logf("pre-terminate: order=%d status=%s bin=%d node=%v claimed=%v",
		order.ID, order.Status, binID, bin.NodeID, bin.ClaimedBy)

	// Try to terminate the confirmed/completed order via TerminateOrder (web UI path).
	// BUG C: This should be rejected, but currently it succeeds.
	err = eng.TerminateOrder(order.ID, "admin")
	if err == nil {
		t.Errorf("BUG C: TerminateOrder accepted order %d in status %q — should reject terminal statuses",
			order.ID, order.Status)

		// Verify the damage: check for spurious return order
		returnOrders, _ := db.ListOrdersByBin(binID, 10)
		for _, o := range returnOrders {
			if o.PayloadDesc == "auto_return" {
				t.Errorf("BUG C (cascade): auto_return order %d created after terminating confirmed order",
					o.ID)
			}
		}

		bin, _ = db.GetBin(binID)
		if bin.ClaimedBy != nil {
			t.Errorf("BUG C (cascade): bin %d claimed_by = %d after terminating confirmed order",
				binID, *bin.ClaimedBy)
		}
	} else {
		t.Logf("correct: TerminateOrder rejected terminal status %q: %v", order.Status, err)
	}
}
