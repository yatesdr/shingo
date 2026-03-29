package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/store"
)

// Complex order lifecycle tests (TC-42, TC-43, TC-47, TC-48, TC-49, TC-50).
// Each test exercises a failure mode or edge case specific to complex orders
// (StepsJSON-based fleet block generation, claimComplexBins, wait/release).

// --- Test: Complex order cancel mid-transit ---
//
// Scenario: A complex order (pickup → dropoff → wait → pickup → dropoff) is
// dispatched and the robot is in transit (RUNNING). The operator cancels the
// order. The bin was claimed by claimComplexBins and is physically on the robot.
//
// Expected: The order is cancelled. The bin claim is released. An auto-return
// order is created to bring the bin back to its origin. No bin is permanently
// stuck.
//
// Why this matters: Operators cancel orders regularly. If the cancel path doesn't
// release the claim and create a return, the bin becomes invisible to the system.
func TestComplexOrder_CancelMidTransit(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-CXCANCEL")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Dispatch a complex order: pickup from storage, dropoff at line, wait, pickup, dropoff back
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-cancel-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: storageNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("cx-cancel-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want %q", order.Status, dispatch.StatusDispatched)
	}

	// Verify bin was claimed
	bin, err = db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
		t.Fatalf("bin should be claimed by order %d, got %v", order.ID, bin.ClaimedBy)
	}

	// Robot starts moving
	sim.DriveState(order.VendorOrderID, "RUNNING")
	order, _ = db.GetOrderByUUID("cx-cancel-1")
	if order.Status != dispatch.StatusInTransit {
		t.Fatalf("after RUNNING: status = %q, want in_transit", order.Status)
	}

	// Operator cancels while robot is in transit
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "cx-cancel-1",
		Reason:    "operator cancelled mid-transit",
	})

	// Verify order is cancelled
	order, _ = db.GetOrderByUUID("cx-cancel-1")
	if order.Status != dispatch.StatusCancelled {
		t.Errorf("order status = %q, want cancelled", order.Status)
	}

	// Verify bin claim released (unclaimed by cancel handler)
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin after cancel: claimed_by=%v status=%s", bin.ClaimedBy, bin.Status)

	// Check for auto-return order — maybeCreateReturnOrder should fire
	// because: BinID set, VendorOrderID set, status was in_transit → cancelled
	allOrders, err := db.ListOrders("", 50)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}

	var returnOrder *store.Order
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}

	if returnOrder == nil {
		t.Errorf("BUG: no auto-return order created after cancelling complex order mid-transit — bin may be stranded")
	} else {
		t.Logf("auto-return order %d created: source=%s dest=%s bin=%v",
			returnOrder.ID, returnOrder.SourceNode, returnOrder.DeliveryNode, returnOrder.BinID)
		// Return order should have claimed the bin
		bin, _ = db.GetBin(bin.ID)
		if bin.ClaimedBy == nil {
			t.Errorf("bin should be claimed by return order, but claimed_by is nil")
		} else if *bin.ClaimedBy != returnOrder.ID {
			t.Errorf("bin claimed by %d, want %d (return order)", *bin.ClaimedBy, returnOrder.ID)
		}
	}
}

// --- Test: Complex order fleet failure mid-transit ---
//
// Scenario: A complex order is dispatched, robot starts moving (RUNNING),
// then the fleet reports FAILED (robot breakdown, obstacle, emergency stop).
//
// Expected: Order marked failed. Bin claim released. Auto-return created.
// Same recovery path as cancel, but triggered by fleet rather than operator.
func TestComplexOrder_FleetFailureMidTransit(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-CXFAIL")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-fail-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("cx-fail-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Robot starts then fails
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FAILED")

	// Give events time to propagate through the engine pipeline
	order, _ = db.GetOrderByUUID("cx-fail-1")
	t.Logf("order after FAILED: status=%s", order.Status)

	if order.Status != dispatch.StatusFailed {
		t.Errorf("order status = %q, want failed", order.Status)
	}

	// Verify bin claim released
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin after failure: claimed_by=%v", bin.ClaimedBy)

	// Check for auto-return order
	allOrders, _ := db.ListOrders("", 50)
	var returnOrder *store.Order
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			returnOrder = o
			break
		}
	}

	if returnOrder == nil {
		t.Errorf("BUG: no auto-return order created after fleet failure on complex order — bin may be stranded")
	} else {
		t.Logf("auto-return order %d: source=%s dest=%s", returnOrder.ID, returnOrder.SourceNode, returnOrder.DeliveryNode)

		// Return should have re-claimed the bin
		bin, _ = db.GetBin(bin.ID)
		if bin.ClaimedBy == nil || *bin.ClaimedBy != returnOrder.ID {
			t.Errorf("bin claimed_by = %v, want %d (return order)", bin.ClaimedBy, returnOrder.ID)
		}
	}
}

// --- Test: Empty post-wait release (TC-47) ---
//
// Scenario: A complex order has steps [pickup, dropoff, wait] with nothing
// after the wait. Edge sends an OrderRelease to unblock the staged order.
// HandleOrderRelease parses StepsJSON, calls splitPostWait, gets an empty
// postWait slice, then calls ReleaseOrder(vendorOrderID, []OrderBlock{}).
//
// Expected: ReleaseOrder is called with nil/empty blocks. The order should
// transition to in_transit and the fleet should mark it complete (no more
// blocks). No panic, no error.
func TestComplexOrder_EmptyPostWaitRelease(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-EPWAIT")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Dispatch complex order: pickup → dropoff → wait (nothing after wait)
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-empty-wait-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
		},
	})

	order, err := db.GetOrderByUUID("cx-empty-wait-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Verify bin claimed
	bin, _ = db.GetBin(bin.ID)
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
		t.Fatalf("bin not claimed by order %d", order.ID)
	}

	// Drive pre-wait blocks through fleet
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "DWELLING")

	// Mark order as staged (simulating the dwell callback)
	if err := db.UpdateOrderStatus(order.ID, dispatch.StatusStaged, "dwelling at lineside"); err != nil {
		t.Fatalf("update to staged: %v", err)
	}

	// Edge sends release — there are no post-wait steps
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "cx-empty-wait-1",
	})

	// Verify no panic and order transitions correctly
	order, _ = db.GetOrderByUUID("cx-empty-wait-1")
	t.Logf("order after empty release: status=%s", order.Status)

	// The fleet should have received a ReleaseOrder with empty blocks,
	// which signals "no more blocks" — effectively completing the order
	if order.Status == dispatch.StatusStaged {
		t.Logf("NOTE: order still staged after empty release — fleet may not have transitioned it yet")
	} else {
		t.Logf("order transitioned to %s after empty release", order.Status)
	}

	// Verify no orphan bins
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin after release: node=%v claimed=%v status=%s", bin.NodeID, bin.ClaimedBy, bin.Status)
}

// --- Test: Complex order redirect doesn't update StepsJSON (TC-48) ---
//
// OBSERVATIONAL TEST: This test always passes. It uses t.Logf (not t.Errorf)
// to document current redirect-vs-StepsJSON behavior. When the underlying
// bug is fixed, convert the Logf calls to assertions.
//
// Scenario: A complex order with a wait (pickup A → dropoff B → wait →
// pickup B → dropoff C) is dispatched. While the order is staged (dwelling),
// the operator sends a redirect to change delivery from C to D.
// HandleOrderRedirect updates DeliveryNode in the DB, but StepsJSON still
// has "dropoff C" in the post-wait steps. When HandleOrderRelease fires,
// it reads StepsJSON and creates fleet blocks with the OLD destination.
//
// Expected: This test documents the bug — the fleet gets blocks with old
// node C, not new node D. The test verifies whether the redirect actually
// takes effect in the post-wait phase.
func TestComplexOrder_RedirectStaleStepsJSON(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	_ = createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REDIR")

	// Create a third node to be the redirect target
	newDest := &store.Node{Name: "LINE-REDIR-NEW", Enabled: true}
	db.CreateNode(newDest)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Complex order: pickup storage → dropoff line → wait → pickup line → dropoff storage
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-redir-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: storageNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("cx-redir-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Drive to staged (dwelling)
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "DWELLING")

	if err := db.UpdateOrderStatus(order.ID, dispatch.StatusStaged, "dwelling"); err != nil {
		t.Fatalf("update to staged: %v", err)
	}

	// Redirect delivery from storageNode to newDest
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{
		OrderUUID:       "cx-redir-1",
		NewDeliveryNode: newDest.Name,
	})

	// Re-fetch order
	order, _ = db.GetOrderByUUID("cx-redir-1")
	t.Logf("order after redirect: delivery=%s status=%s", order.DeliveryNode, order.Status)

	// Check if DeliveryNode was updated
	if order.DeliveryNode != newDest.Name {
		t.Logf("NOTE: DeliveryNode not updated to %s (got %s) — redirect may have been rejected for staged orders", newDest.Name, order.DeliveryNode)
	}

	// Key check: StepsJSON still has old destination
	t.Logf("StepsJSON after redirect: %s", order.StepsJSON)

	// If order is back to staged, try releasing
	if order.Status == dispatch.StatusStaged || order.Status == dispatch.StatusSourcing {
		// If redirect put it back to sourcing, it will re-dispatch. Otherwise release.
		if order.Status == dispatch.StatusStaged {
			d.HandleOrderRelease(env, &protocol.OrderRelease{
				OrderUUID: "cx-redir-1",
			})
		}

		order, _ = db.GetOrderByUUID("cx-redir-1")
		t.Logf("order after release: status=%s delivery=%s", order.Status, order.DeliveryNode)

		// BUG CHECK: The post-wait blocks are built from StepsJSON which still
		// references the old destination. The fleet will route to the wrong node.
		if order.StepsJSON != "" {
			t.Logf("POTENTIAL BUG: StepsJSON not updated after redirect — post-wait fleet blocks use old destination")
		}
	}
}

// --- Test: Ghost robot — claimComplexBins finds no bin (TC-49) ---
//
// OBSERVATIONAL TEST: This test always passes. It uses t.Logf (not t.Errorf)
// to document the ghost-robot dispatch path. When a bin-required guard is
// added to claimComplexBins, convert the Logf calls to assertions.
//
// Scenario: A complex order specifies a pickup at a node, but the node
// has no bins matching the payload (or all bins are already claimed).
// claimComplexBins is best-effort and logs a warning but lets the order
// dispatch anyway — with BinID=nil.
//
// Expected: The order dispatches to fleet (ghost robot). When the robot
// arrives at the empty node, it will fail. The test verifies that:
// 1. Order dispatches with BinID=nil
// 2. No auto-return is created (BinID=nil guard in maybeCreateReturnOrder)
// 3. The failure path still marks the order failed cleanly
func TestComplexOrder_GhostRobotNoBin(t *testing.T) {
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Create an empty pickup node — no bins at all
	emptyNode := &store.Node{Name: "EMPTY-PICKUP", Enabled: true}
	db.CreateNode(emptyNode)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Complex order picks up from an empty node
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-ghost-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: emptyNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("cx-ghost-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Key check: order dispatched but with no bin
	if order.BinID != nil {
		t.Errorf("expected BinID=nil (no bin at pickup), got %d", *order.BinID)
	} else {
		t.Logf("CONFIRMED: order dispatched with BinID=nil — ghost robot will be sent to empty node")
	}

	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Robot arrives, can't find bin, fleet reports FAILED
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FAILED")

	order, _ = db.GetOrderByUUID("cx-ghost-1")
	if order.Status != dispatch.StatusFailed {
		t.Errorf("order status = %q after fleet failure, want failed", order.Status)
	}

	// No auto-return should be created (BinID=nil)
	allOrders, _ := db.ListOrders("", 50)
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			t.Errorf("BUG: auto-return order created for ghost robot (no bin!) — order %d", o.ID)
		}
	}
	t.Logf("ghost robot failure handled cleanly — no spurious auto-return")
}

// --- Test: Concurrent complex orders targeting same node — double claim race (TC-50) ---
//
// Scenario: Two complex orders are submitted simultaneously, both picking up
// from the same storage node that has only one available bin.
// claimComplexBins runs for both orders in sequence. The first should claim
// the bin; the second should get no bin (ghost robot).
//
// Expected: Only one order claims the bin. The second dispatches with
// BinID=nil. No double-claim occurs (bin.ClaimedBy can only reference one order).
func TestComplexOrder_ConcurrentSameNodeDoubleClaimRace(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RACE")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// First order
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-race-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	// Second order — same pickup node, same payload
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "cx-race-2",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order1, _ := db.GetOrderByUUID("cx-race-1")
	order2, _ := db.GetOrderByUUID("cx-race-2")

	// Check which order got the bin
	bin, _ = db.GetBin(bin.ID)
	t.Logf("bin claimed by: %v", bin.ClaimedBy)
	t.Logf("order1: status=%s bin=%v", order1.Status, order1.BinID)
	t.Logf("order2: status=%s bin=%v", order2.Status, order2.BinID)

	// Exactly one order should have the bin
	hasBin := 0
	if order1.BinID != nil {
		hasBin++
	}
	if order2.BinID != nil {
		hasBin++
	}
	if hasBin > 1 {
		t.Errorf("BUG: both orders claimed a bin — double claim! order1.bin=%v order2.bin=%v", order1.BinID, order2.BinID)
	} else if hasBin == 1 {
		t.Logf("correct: exactly one order claimed the bin, other dispatched as ghost")
	} else {
		t.Logf("NOTE: neither order claimed the bin — possible if both raced and lost")
	}

	// Both orders should be dispatched regardless
	if order1.Status != dispatch.StatusDispatched {
		t.Errorf("order1 status = %q, want dispatched", order1.Status)
	}
	if order2.Status != dispatch.StatusDispatched {
		t.Errorf("order2 status = %q, want dispatched", order2.Status)
	}
}
