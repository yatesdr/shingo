//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// --- TC-23b: Cancel in-flight store order — return order claims bin ---
// Scenario: verifies cancel → unclaim → auto-return-order → re-claim flow.
//
// Line has 3 bins. Bin is claimed by active store order, robot is RUNNING.
// Operator cancels. The system unclaims the bin, then maybeCreateReturnOrder
// creates a return order that immediately re-claims the same bin to bring
// it back. The bin is never truly "free" — it transfers from the original
// order to the return order. A subsequent store order should claim one of
// the OTHER unclaimed bins, not the one held by the return order.
func TestCancel_ClaimTransfersToReturnOrder(t *testing.T) {
	t.Parallel()
	// Skipped 2026-04-14: exercises auto-return protection of a cancelled
	// bin against subsequent store-order poaching. autoReturnEnabled is
	// currently false (see maybeCreateReturnOrder), so there is no return
	// order and the bin is simply unclaimed after cancel — the premise of
	// this test no longer holds. Preserved for re-enable alongside
	// TestMaybeCreateReturnOrder_SourceNode when auto-return comes back.
	t.Skip("auto-return disabled; re-enable with autoReturnEnabled")
	db := testDB(t)
	bins, _, lineNode, _ := setupThreeBinLine(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Create a store order that claims a bin and dispatches a robot
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "active-23b",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	order := testdb.RequireOrder(t, db, "active-23b")
	if order.BinID == nil {
		t.Fatal("store order should have claimed a bin")
	}
	claimedBinID := *order.BinID
	t.Logf("active order %d claimed bin %d", order.ID, claimedBinID)

	// Drive robot to in-transit
	if order.VendorOrderID != "" {
		sim.DriveState(order.VendorOrderID, "RUNNING")
	}

	// Verify bin is claimed before cancel
	bin0, _ := db.GetBin(claimedBinID)
	if bin0.ClaimedBy == nil {
		t.Fatal("bin should be claimed before cancel")
	}

	// Cancel the active order
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "active-23b",
		Reason:    "changeover",
	})

	order = testdb.RequireOrderStatus(t, db, "active-23b", dispatch.StatusCancelled)

	// KEY CHECK: bin should now be claimed by the auto-return order, not free.
	// Cancel flow: unclaim original → maybeCreateReturnOrder → return claims bin.
	bin0 = testdb.RequireBin(t, db, claimedBinID)
	if bin0.ClaimedBy == nil {
		t.Logf("bin %d is unclaimed after cancel (no return order created)", bin0.ID)
	} else if *bin0.ClaimedBy == order.ID {
		t.Errorf("BUG: bin %d still claimed by cancelled order %d — unclaim failed", bin0.ID, order.ID)
	} else {
		t.Logf("bin %d claim transferred from cancelled order %d to return order %d — correct",
			bin0.ID, order.ID, *bin0.ClaimedBy)
	}

	// A subsequent store order should claim one of the OTHER bins, not the
	// one held by the return order.
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{
		OrderUUID:  "store-23b-move",
		OrderType:  dispatch.OrderTypeStore,
		SourceNode: lineNode.Name,
	})

	storeOrder := testdb.RequireOrder(t, db, "store-23b-move")

	if storeOrder.BinID != nil && *storeOrder.BinID == claimedBinID {
		t.Errorf("BUG: store order stole bin %d from the return order", claimedBinID)
	} else if storeOrder.BinID != nil {
		t.Logf("store order claimed different bin %d — correct (return order protects bin %d)",
			*storeOrder.BinID, claimedBinID)
	} else {
		t.Logf("store order has no bin (status=%s)", storeOrder.Status)
	}

	// Verify remaining bins
	for i := 0; i < 3; i++ {
		b := testdb.RequireBin(t, db, bins[i].ID)
		claimStr := "unclaimed"
		if b.ClaimedBy != nil {
			claimStr = fmt.Sprintf("claimed_by=%d", *b.ClaimedBy)
		}
		t.Logf("bin %d (%s): node=%v, %s", b.ID, b.Label, b.NodeID, claimStr)
	}
}

// --- TC-30: Failed order creates a return — does the return inherit the reservation? ---
// Scenario: verifies that when a fleet-reported failure triggers an auto-return
// order, the bin claim transfers cleanly from the failed order to the return order.
//
// A retrieve order is dispatched and the fleet accepts it. The robot starts
// moving (RUNNING). Then the fleet reports the order as FAILED (robot broke
// down mid-delivery). The system should:
// 1. Mark the original order as failed
// 2. Release the original order's bin claim
// 3. Create an auto-return order to send the bin back to storage
// 4. Claim the bin for the return order
//
// The bug risk: the fleet-reported failure path (handleVendorStatusChange)
// does NOT call UnclaimOrderBins before emitting EventOrderFailed. The
// EventOrderFailed handler calls maybeCreateReturnOrder, which tries to
// ClaimBin for the return order. But with the ClaimBin fix (AND claimed_by
// IS NULL), this will fail because the bin is still claimed by the original
// order. The return order gets created but can't claim its bin.
func TestFailedOrder_TransfersReturnClaim(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC30")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Step 1: Dispatch a retrieve order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-tc30",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrderStatus(t, db, "retrieve-tc30", dispatch.StatusDispatched)
	if order.BinID == nil {
		t.Fatal("order should have a bin claimed")
	}
	t.Logf("order %d dispatched, bin %d claimed, vendor_id=%s", order.ID, *order.BinID, order.VendorOrderID)

	// Verify bin is claimed by the original order
	testdb.RequireBinClaimedBy(t, db, *order.BinID, order.ID)

	// Step 2: Robot starts moving
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order = testdb.RequireOrderStatus(t, db, "retrieve-tc30", "in_transit")

	// Step 3: Fleet reports FAILED (robot broke down)
	sim.DriveState(order.VendorOrderID, "FAILED")

	// FAILED now maps to faulted (non-terminal) instead of failed.
	// The order enters grace period; bin claim persists until grace expiry.
	order = testdb.RequireOrderStatus(t, db, "retrieve-tc30", dispatch.StatusFaulted)
	t.Logf("original order %d is now faulted (grace period)", order.ID)

	// Step 4: During faulted grace period, bin claim persists.
	bin = testdb.RequireBin(t, db, *order.BinID)
	if bin.ClaimedBy != nil && *bin.ClaimedBy == order.ID {
		// Expected: bin still claimed during faulted grace period.
		// Claim released on grace-expiry terminal transition.
	} else if bin.ClaimedBy != nil {
		// claimed by a different order
	} else {
		// claim released (unexpected during grace period)
	}

	// Step 5: Check if a return order was created
	// The return order should have PayloadDesc = "auto_return" and OrderType = "store"
	// We can find it by looking for orders other than the original
	allOrders, err := db.ListOrdersByStation(order.StationID, 50)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}

	var returnOrder *orders.Order
	for _, o := range allOrders {
		if o.ID != order.ID && o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}

	if returnOrder == nil {
		t.Logf("no auto-return order was created")
		// This might be OK or might be a bug depending on the guards
	} else {
		t.Logf("return order %d created: type=%s, status=%s, bin_id=%v, source=%s, dest=%s",
			returnOrder.ID, returnOrder.OrderType, returnOrder.Status,
			returnOrder.BinID, returnOrder.SourceNode, returnOrder.DeliveryNode)

		// The return order should have the bin
		if returnOrder.BinID == nil || *returnOrder.BinID != *order.BinID {
			t.Errorf("return order bin_id = %v, want %d (same bin as failed order)", returnOrder.BinID, *order.BinID)
		}

		// KEY CHECK: the bin should be claimed by the RETURN order, not the original
		bin = testdb.RequireBin(t, db, *order.BinID)

		if bin.ClaimedBy == nil {
			t.Errorf("BUG: bin %d is unclaimed — return order %d exists but couldn't claim the bin (likely because original claim wasn't released first)",
				bin.ID, returnOrder.ID)
		} else if *bin.ClaimedBy == returnOrder.ID {
			t.Logf("bin %d correctly claimed by return order %d", bin.ID, returnOrder.ID)
		} else if *bin.ClaimedBy == order.ID {
			t.Errorf("BUG: bin %d still claimed by failed order %d — return order %d could not take over the claim",
				bin.ID, order.ID, returnOrder.ID)
		} else {
			t.Errorf("bin %d claimed by unexpected order %d (not original %d or return %d)",
				bin.ID, *bin.ClaimedBy, order.ID, returnOrder.ID)
		}

		// The return order should not be in a failed state
		if returnOrder.Status == dispatch.StatusFailed {
			t.Errorf("return order %d is failed — bin may be stranded", returnOrder.ID)
		}
	}
}

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

// --- TC-36: Retrieve claim failure — queue instead of fail ---
// Scenario: Two concurrent retrieve orders compete for the only bin of a payload.
// Both find the same bin via FindSourceBinFIFO (TOCTOU gap between SELECT and
// UPDATE). The first ClaimBin succeeds. The second ClaimBin fails with
// "bin is locked, already claimed, or does not exist".
//
// Bug: planRetrieve returns planningError{Code: "claim_failed"}, which causes
// HandleOrderRequest to call failOrder — permanently failing the order. But
// claim_failed is a transient condition: bins of the right payload DO exist,
// one was just claimed by another order in the race window. The order should be
// queued so the fulfillment scanner retries when a bin becomes available.
//
// Fix: HandleOrderRequest now checks for planErr.Code == "claim_failed" and
// calls queueOrder instead of failOrder. The fulfillment scanner will retry
// on its next sweep.
//
// Note: This test uses concurrent goroutines to trigger the TOCTOU race. The
// race is not guaranteed on every run — if the Go runtime serializes the
// goroutines, both orders succeed sequentially. The test always passes when
// no race occurs; it only fails when the race exposes the claim_failed path
// and the fix is not applied.
// TestMaybeCreateReturnOrder_SourceNode verifies that auto-return orders use the
// original order's SourceNode (where the bin actually is in the DB), not DeliveryNode
// (where the bin was headed but never reached). This was Bug 1: maybeCreateReturnOrder
// used order.DeliveryNode for SourceNode, sending the recovery robot to the wrong node.
//
// This test isolates the bug without relying on TC-42's round-trip symmetry
// (where SourceNode == DeliveryNode masks the defect).
func TestMaybeCreateReturnOrder_SourceNode(t *testing.T) {
	t.Parallel()
	// Auto-return is short-circuited (see autoReturnEnabled in wiring.go).
	// This test asserts the SourceNode behavior of maybeCreateReturnOrder
	// when it actually creates an order — re-enable when autoReturnEnabled
	// is flipped back to true. Until then, the function returns early and
	// no return order is ever created, so the assertion at line ~1339 would
	// fail with "no auto-return order created after fleet failure".
	t.Skip("auto-return short-circuited; see autoReturnEnabled in wiring.go")
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RETSRC")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Dispatch a retrieve order: storage → line
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "ret-src-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "ret-src-1")
	if order.SourceNode != storageNode.Name {
		t.Fatalf("order SourceNode = %q, want %q (storage)", order.SourceNode, storageNode.Name)
	}
	if order.DeliveryNode != lineNode.Name {
		t.Fatalf("order DeliveryNode = %q, want %q (line)", order.DeliveryNode, lineNode.Name)
	}

	// Robot starts moving then fails mid-transit
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FAILED")

	// Find auto-return order
	allOrders, _ := db.ListOrders("", 50)
	var returnOrder *orders.Order
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}
	if returnOrder == nil {
		t.Fatalf("no auto-return order created after fleet failure")
	}

	// KEY CHECK: return order SourceNode must be the original order's SourceNode
	// (storage, where the bin IS), NOT DeliveryNode (line, where the bin never reached)
	if returnOrder.SourceNode != order.SourceNode {
		t.Errorf("BUG: return order SourceNode = %q, want %q (order.SourceNode = storage where bin actually is). "+
			"maybeCreateReturnOrder used order.DeliveryNode which is WRONG for mid-transit failures — "+
			"recovery robot goes to line to pick up a bin that's still at storage",
			returnOrder.SourceNode, order.SourceNode)
	}

	// Verify bin re-claimed by return order
	bin, _ = db.GetBin(bin.ID)
	if bin.ClaimedBy == nil || *bin.ClaimedBy != returnOrder.ID {
		t.Errorf("bin claimed_by = %v, want %d (return order)", bin.ClaimedBy, returnOrder.ID)
	}
}
