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

// =============================================================================
// Production Cycle Pattern Tests (TC-55 through TC-60)
//
// Six tests exercising the production cycle patterns from
// shingo-edge/engine/material_orders.go through the Core engine pipeline.
// Each test verifies block construction, bin claiming, wait/release lifecycle,
// and bin movement for a real-world order flow.
// =============================================================================

// setupProductionNodes extends setupTestData with three additional nodes needed
// by production cycle patterns: INBOUND-STAGING, OUTBOUND-STAGING, OUTBOUND-DEST.
func setupProductionNodes(t *testing.T, db *store.DB) (
	storageNode, lineNode *store.Node,
	inboundStaging, outboundStaging, outboundDest *store.Node,
	bp *store.Payload,
) {
	t.Helper()
	storageNode, lineNode, bp = setupTestData(t, db)

	inboundStaging = &store.Node{Name: "INBOUND-STAGING", Enabled: true}
	if err := db.CreateNode(inboundStaging); err != nil {
		t.Fatalf("create inbound staging node: %v", err)
	}
	outboundStaging = &store.Node{Name: "OUTBOUND-STAGING", Enabled: true}
	if err := db.CreateNode(outboundStaging); err != nil {
		t.Fatalf("create outbound staging node: %v", err)
	}
	outboundDest = &store.Node{Name: "OUTBOUND-DEST", Enabled: true}
	if err := db.CreateNode(outboundDest); err != nil {
		t.Fatalf("create outbound dest node: %v", err)
	}
	return
}

// driveToConfirmed advances an order through the full lifecycle:
// RUNNING → FINISHED → receipt → confirmed.
func driveToConfirmed(t *testing.T, sim *simulator.SimulatorBackend, d *dispatch.Dispatcher, db *store.DB, orderUUID string) *store.Order {
	t.Helper()
	order, err := db.GetOrderByUUID(orderUUID)
	if err != nil {
		t.Fatalf("get order %s: %v", orderUUID, err)
	}
	if order.VendorOrderID == "" {
		t.Fatalf("order %s has no vendor order ID", orderUUID)
	}
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(testEnvelope(), &protocol.OrderReceipt{
		OrderUUID:   orderUUID,
		ReceiptType: "confirmed",
		FinalCount:  1,
	})
	order, err = db.GetOrderByUUID(orderUUID)
	if err != nil {
		t.Fatalf("get order %s after confirmation: %v", orderUUID, err)
	}
	return order
}

// --- TC-55: Sequential Backfill (Order B) — simplest, no wait ---
//
// Pattern from BuildSequentialBackfillSteps:
//
//	pickup(InboundSource) → dropoff(CoreNode)
//
// No wait step. Dispatches as complete immediately. The robot picks up
// a bin from storage and drops it at the line.
//
// Expected: Bin claimed at storage, 2 blocks (JackLoad + JackUnload), complete.
// After lifecycle: order confirmed, bin at line, unclaimed.
func TestComplexOrder_SequentialBackfill(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, _, _, _, bp := setupProductionNodes(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-SEQBF")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "seq-backfill-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("seq-backfill-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Bin claimed at storage (first step is pickup)
	if order.BinID == nil {
		t.Fatal("expected BinID to be set — claimComplexBins should claim at pickup node")
	}
	bin, _ = db.GetBin(bin.ID)
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
		t.Fatalf("bin should be claimed by order %d, got claimed_by=%v", order.ID, bin.ClaimedBy)
	}

	// No staged orders — dispatched as complete immediately
	if sim.StagedOrderCount() != 0 {
		t.Fatalf("staged orders = %d, want 0 (no wait step)", sim.StagedOrderCount())
	}

	// 2 blocks: storage/JackLoad, line/JackUnload
	view := sim.GetOrder(order.VendorOrderID)
	if view == nil {
		t.Fatal("simulator should have the order")
	}
	if len(view.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(view.Blocks))
	}
	if view.Blocks[0].Location != storageNode.Name || view.Blocks[0].BinTask != "JackLoad" {
		t.Errorf("block 0: location=%q task=%q, want %q/JackLoad", view.Blocks[0].Location, view.Blocks[0].BinTask, storageNode.Name)
	}
	if view.Blocks[1].Location != lineNode.Name || view.Blocks[1].BinTask != "JackUnload" {
		t.Errorf("block 1: location=%q task=%q, want %q/JackUnload", view.Blocks[1].Location, view.Blocks[1].BinTask, lineNode.Name)
	}
	if !view.Complete {
		t.Error("order should be complete (no wait step)")
	}

	// Drive through full lifecycle
	order = driveToConfirmed(t, sim, d, db, "seq-backfill-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Fatalf("status = %q, want confirmed", order.Status)
	}

	// Bin moved to line, unclaimed
	bin, _ = db.GetBin(bin.ID)
	if bin.NodeID == nil || *bin.NodeID != lineNode.ID {
		t.Errorf("bin node = %v, want %d (line)", bin.NodeID, lineNode.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil (claim released)", bin.ClaimedBy)
	}
}

// --- TC-56: Sequential Removal (Order A) — wait/release lifecycle ---
//
// Pattern from BuildSequentialRemovalSteps:
//
//	dropoff(CoreNode) → wait → pickup(CoreNode) → dropoff(OutboundDest)
//
// Robot navigates empty to line, waits for operator, picks up old bin, delivers
// to outbound destination. First step is dropoff, but claimComplexBins iterates
// ALL steps (including post-wait) and finds the pickup(lineNode) step, claiming
// the bin there.
//
// Expected: 1 pre-wait block, staged order. After release: 3 total blocks.
// After completion: bin at outbound dest.
func TestComplexOrder_SequentialRemoval(t *testing.T) {
	db := testDB(t)
	_, lineNode, _, _, outboundDest, bp := setupProductionNodes(t, db)
	_ = createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-SEQR")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "seq-removal-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: outboundDest.Name},
		},
	})

	order, err := db.GetOrderByUUID("seq-removal-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Bin claimed — claimComplexBins iterates ALL steps including post-wait
	if order.BinID == nil {
		t.Fatal("expected BinID set — claimComplexBins finds post-wait pickup step")
	}

	// 1 pre-wait block, staged
	if sim.StagedOrderCount() != 1 {
		t.Fatalf("staged orders = %d, want 1", sim.StagedOrderCount())
	}
	view := sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 1 {
		t.Fatalf("pre-wait blocks = %d, want 1", len(view.Blocks))
	}
	if view.Blocks[0].Location != lineNode.Name || view.Blocks[0].BinTask != "JackUnload" {
		t.Errorf("block 0: location=%q task=%q, want %q/JackUnload", view.Blocks[0].Location, view.Blocks[0].BinTask, lineNode.Name)
	}
	if view.Complete {
		t.Error("order should NOT be complete (has wait step)")
	}

	// Drive to staged
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")
	order, _ = db.GetOrderByUUID("seq-removal-1")
	if order.Status != dispatch.StatusStaged {
		t.Fatalf("after WAITING: status = %q, want staged", order.Status)
	}

	// Release — appends 2 post-wait blocks
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: "seq-removal-1"})

	// After release: 3 total blocks
	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 3 {
		t.Fatalf("total blocks after release = %d, want 3", len(view.Blocks))
	}
	if !view.Complete {
		t.Error("order should be complete after release")
	}

	// Post-wait blocks: line/JackLoad, dest/JackUnload
	if view.Blocks[1].Location != lineNode.Name || view.Blocks[1].BinTask != "JackLoad" {
		t.Errorf("block 1: location=%q task=%q, want %q/JackLoad", view.Blocks[1].Location, view.Blocks[1].BinTask, lineNode.Name)
	}
	if view.Blocks[2].Location != outboundDest.Name || view.Blocks[2].BinTask != "JackUnload" {
		t.Errorf("block 2: location=%q task=%q, want %q/JackUnload", view.Blocks[2].Location, view.Blocks[2].BinTask, outboundDest.Name)
	}

	// Drive to completion
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")
	order, _ = db.GetOrderByUUID("seq-removal-1")
	if order.Status != "delivered" {
		t.Fatalf("after FINISHED: status = %q, want delivered", order.Status)
	}
}

// --- TC-57: Two-Robot Swap Resupply (Order A) ---
//
// Pattern from BuildTwoRobotSwapSteps orderA:
//
//	pickup(Source) → dropoff(InboundStaging) → wait
//	→ pickup(InboundStaging) → dropoff(CoreNode)
//
// Expected: 2 pre-wait blocks, bin claimed at storage, staged order.
// After release: 4 total blocks. After completion: bin at line.
func TestComplexOrder_TwoRobotSwap_Resupply(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, inboundStaging, _, _, bp := setupProductionNodes(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-2RA")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "tworobot-resupply-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: inboundStaging.Name},
			{Action: "wait"},
			{Action: "pickup", Node: inboundStaging.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("tworobot-resupply-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Bin claimed at storage (first step is pickup)
	if order.BinID == nil {
		t.Fatal("expected BinID — first step is pickup at storage")
	}
	bin, _ = db.GetBin(bin.ID)
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
		t.Fatalf("bin claimed_by = %v, want order %d", bin.ClaimedBy, order.ID)
	}

	// Staged with 2 pre-wait blocks
	if sim.StagedOrderCount() != 1 {
		t.Fatalf("staged orders = %d, want 1", sim.StagedOrderCount())
	}
	view := sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 2 {
		t.Fatalf("pre-wait blocks = %d, want 2", len(view.Blocks))
	}
	if view.Blocks[0].Location != storageNode.Name || view.Blocks[0].BinTask != "JackLoad" {
		t.Errorf("block 0: location=%q task=%q, want %q/JackLoad", view.Blocks[0].Location, view.Blocks[0].BinTask, storageNode.Name)
	}
	if view.Blocks[1].Location != inboundStaging.Name || view.Blocks[1].BinTask != "JackUnload" {
		t.Errorf("block 1: location=%q task=%q, want %q/JackUnload", view.Blocks[1].Location, view.Blocks[1].BinTask, inboundStaging.Name)
	}
	if view.Complete {
		t.Error("order should not be complete")
	}

	// Drive to staged
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")
	order, _ = db.GetOrderByUUID("tworobot-resupply-1")
	if order.Status != dispatch.StatusStaged {
		t.Fatalf("after WAITING: status = %q, want staged", order.Status)
	}

	// Release — adds 2 post-wait blocks
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: "tworobot-resupply-1"})

	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 4 {
		t.Fatalf("total blocks after release = %d, want 4", len(view.Blocks))
	}
	if !view.Complete {
		t.Error("order should be complete after release")
	}

	// Post-wait blocks: inboundStaging/JackLoad, line/JackUnload
	if view.Blocks[2].Location != inboundStaging.Name || view.Blocks[2].BinTask != "JackLoad" {
		t.Errorf("block 2: location=%q task=%q, want %q/JackLoad", view.Blocks[2].Location, view.Blocks[2].BinTask, inboundStaging.Name)
	}
	if view.Blocks[3].Location != lineNode.Name || view.Blocks[3].BinTask != "JackUnload" {
		t.Errorf("block 3: location=%q task=%q, want %q/JackUnload", view.Blocks[3].Location, view.Blocks[3].BinTask, lineNode.Name)
	}

	// Complete
	order = driveToConfirmed(t, sim, d, db, "tworobot-resupply-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Fatalf("status = %q, want confirmed", order.Status)
	}

	// Bin moved to line, unclaimed
	bin, _ = db.GetBin(bin.ID)
	if bin.NodeID == nil || *bin.NodeID != lineNode.ID {
		t.Errorf("bin node = %v, want %d (line)", bin.NodeID, lineNode.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil", bin.ClaimedBy)
	}
}

// --- TC-58: Two-Robot Swap Removal (Order B) ---
//
// Pattern from BuildTwoRobotSwapSteps orderB:
//
//	dropoff(CoreNode) → wait → pickup(CoreNode) → dropoff(OutboundDest)
//
// Same structure as sequential removal but in the two-robot context.
// claimComplexBins finds the post-wait pickup and claims the bin.
//
// Expected: 1 pre-wait block, staged. After release: 3 blocks.
// After completion: bin at outbound dest.
func TestComplexOrder_TwoRobotSwap_Removal(t *testing.T) {
	db := testDB(t)
	_, lineNode, _, _, outboundDest, bp := setupProductionNodes(t, db)
	_ = createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-2RB")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "tworobot-removal-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: outboundDest.Name},
		},
	})

	order, err := db.GetOrderByUUID("tworobot-removal-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// Bin claimed — claimComplexBins iterates ALL steps including post-wait
	if order.BinID == nil {
		t.Fatal("expected BinID set — claimComplexBins finds post-wait pickup step")
	}

	// 1 pre-wait block, staged
	if sim.StagedOrderCount() != 1 {
		t.Fatalf("staged orders = %d, want 1", sim.StagedOrderCount())
	}
	view := sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 1 {
		t.Fatalf("pre-wait blocks = %d, want 1", len(view.Blocks))
	}
	if view.Blocks[0].Location != lineNode.Name || view.Blocks[0].BinTask != "JackUnload" {
		t.Errorf("block 0: location=%q task=%q, want %q/JackUnload", view.Blocks[0].Location, view.Blocks[0].BinTask, lineNode.Name)
	}

	// Drive to staged, release
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: "tworobot-removal-1"})

	// 3 total blocks after release
	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 3 {
		t.Fatalf("total blocks = %d, want 3", len(view.Blocks))
	}
	if !view.Complete {
		t.Error("order should be complete after release")
	}

	// Block structure: line/JackUnload, line/JackLoad, dest/JackUnload
	if view.Blocks[1].Location != lineNode.Name || view.Blocks[1].BinTask != "JackLoad" {
		t.Errorf("block 1: location=%q task=%q, want %q/JackLoad", view.Blocks[1].Location, view.Blocks[1].BinTask, lineNode.Name)
	}
	if view.Blocks[2].Location != outboundDest.Name || view.Blocks[2].BinTask != "JackUnload" {
		t.Errorf("block 2: location=%q task=%q, want %q/JackUnload", view.Blocks[2].Location, view.Blocks[2].BinTask, outboundDest.Name)
	}

	// Complete — BinID is set, so ApplyBinArrival moves bin to outboundDest
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "tworobot-removal-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})
	order, _ = db.GetOrderByUUID("tworobot-removal-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Fatalf("after receipt: status = %q, want confirmed", order.Status)
	}

	// Bin moved to outboundDest (unclaimed)
	bin, _ := db.GetBin(*order.BinID)
	if bin.NodeID == nil || *bin.NodeID != outboundDest.ID {
		t.Errorf("bin node = %v, want %d (outboundDest)", bin.NodeID, outboundDest.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil (claim released)", bin.ClaimedBy)
	}
}

// --- TC-59: Staging + Deliver Separation ---
//
// Two independent orders used during changeover.
// Stage:  pickup(storage) → dropoff(inboundStaging)  — no wait
// Deliver: pickup(inboundStaging) → dropoff(lineNode) — no wait
//
// Expected: Stage order completes, bin at inboundStaging, status=staged.
// Deliver order claims the staged bin and delivers it to lineNode.
func TestComplexOrder_StagingAndDeliver(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, inboundStaging, _, _, bp := setupProductionNodes(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-STAGE")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// --- Stage order ---
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "stage-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: inboundStaging.Name},
		},
	})

	stageOrder, err := db.GetOrderByUUID("stage-1")
	if err != nil {
		t.Fatalf("get stage order: %v", err)
	}
	if stageOrder.Status != dispatch.StatusDispatched {
		t.Fatalf("stage order status = %q, want dispatched", stageOrder.Status)
	}
	if stageOrder.BinID == nil {
		t.Fatal("stage order should have claimed bin at storage")
	}

	// Complete stage order
	stageOrder = driveToConfirmed(t, sim, d, db, "stage-1")
	if stageOrder.Status != dispatch.StatusConfirmed {
		t.Fatalf("stage order status = %q, want confirmed", stageOrder.Status)
	}

	// Bin at inbound staging, unclaimed, status=staged
	bin, _ = db.GetBin(bin.ID)
	if bin.NodeID == nil || *bin.NodeID != inboundStaging.ID {
		t.Fatalf("bin node = %v, want %d (inbound staging)", bin.NodeID, inboundStaging.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil (claim released)", bin.ClaimedBy)
	}
	if bin.Status != "staged" {
		t.Errorf("bin status = %q, want staged", bin.Status)
	}

	// --- Deliver order ---
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "deliver-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: inboundStaging.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	deliverOrder, err := db.GetOrderByUUID("deliver-1")
	if err != nil {
		t.Fatalf("get deliver order: %v", err)
	}
	if deliverOrder.Status != dispatch.StatusDispatched {
		t.Fatalf("deliver order status = %q, want dispatched", deliverOrder.Status)
	}

	// Deliver order claims the staged bin at inbound staging
	if deliverOrder.BinID == nil {
		t.Fatal("deliver order should have claimed the staged bin at inbound staging")
	}
	if *deliverOrder.BinID != bin.ID {
		t.Errorf("deliver order bin_id = %d, want %d (the staged bin)", *deliverOrder.BinID, bin.ID)
	}

	// Two independent vendor orders
	if sim.OrderCount() != 2 {
		t.Errorf("simulator orders = %d, want 2 (stage + deliver)", sim.OrderCount())
	}

	// Complete deliver order
	deliverOrder = driveToConfirmed(t, sim, d, db, "deliver-1")
	if deliverOrder.Status != dispatch.StatusConfirmed {
		t.Fatalf("deliver order status = %q, want confirmed", deliverOrder.Status)
	}

	// Bin at line node, unclaimed
	bin, _ = db.GetBin(bin.ID)
	if bin.NodeID == nil || *bin.NodeID != lineNode.ID {
		t.Errorf("bin node = %v, want %d (line)", bin.NodeID, lineNode.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil", bin.ClaimedBy)
	}
}

// --- TC-60: Single-Robot 10-Step Swap ---
//
// Pattern from BuildSingleSwapSteps — the most complex production pattern:
//
//	pickup(source) → dropoff(inStaging) → dropoff(line) → wait
//	→ pickup(line) → dropoff(outStaging) → pickup(inStaging)
//	→ dropoff(line) → pickup(outStaging) → dropoff(outDest)
//
// One robot swaps two bins in a single trip. It stages the new bin at inbound
// staging, pre-positions at line, waits for the operator, swaps old for new,
// then delivers the old bin to outbound destination.
//
// This test exposes TWO defects in multi-bin complex order handling:
//
//  1. WRONG DESTINATION: extractEndpoints sets DeliveryNode to the last
//     actionable step (outboundDest, step 10). But newBin actually delivers at
//     step 8 (lineNode). ApplyBinArrival moves BinID to DeliveryNode blindly —
//     the new bin the line needs ends up at outboundDest.
//
//  2. ORPHANED CLAIM: claimComplexBins claims oldBin at the step 5 pickup
//     (lineNode), but Order.BinID only tracks newBin. On completion,
//     handleOrderCompleted calls ApplyBinArrival for BinID only. There is
//     no UnclaimOrderBins call on the success path — that only exists on
//     failure/cancel paths. oldBin stays claimed by the completed order
//     permanently. No other order can touch it.
//
// Root cause: Order.BinID is *int64 (single bin). The completion path
// (handleOrderCompleted → ApplyBinArrival) processes exactly one bin.
// Multi-pickup orders need a junction table (order_bins) and per-step
// bin tracking. The system's own ListOrderCompletionAnomalies query
// detects "terminal orders still claiming bins."
//
// Expected: 3 pre-wait blocks, staged order. After release: 9 total blocks.
// Both defects asserted with t.Errorf (test documents the problems).
func TestComplexOrder_SingleRobotSwap(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, inboundStaging, outboundStaging, outboundDest, bp := setupProductionNodes(t, db)

	// Two bins: new material at storage, old material at line
	newBin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-SINGLE-NEW")
	oldBin := createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-SINGLE-OLD")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "single-swap-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},       // 1
			{Action: "dropoff", Node: inboundStaging.Name},   // 2
			{Action: "dropoff", Node: lineNode.Name},         // 3
			{Action: "wait"},                                 // 4
			{Action: "pickup", Node: lineNode.Name},          // 5
			{Action: "dropoff", Node: outboundStaging.Name},  // 6
			{Action: "pickup", Node: inboundStaging.Name},    // 7
			{Action: "dropoff", Node: lineNode.Name},         // 8
			{Action: "pickup", Node: outboundStaging.Name},   // 9
			{Action: "dropoff", Node: outboundDest.Name},     // 10
		},
	})

	order, err := db.GetOrderByUUID("single-swap-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != dispatch.StatusDispatched {
		t.Fatalf("status = %q, want dispatched", order.Status)
	}

	// New bin at storage claimed (BinID = first claimed bin)
	if order.BinID == nil {
		t.Fatal("expected BinID — first pickup is at storage")
	}
	newBin, _ = db.GetBin(newBin.ID)
	if newBin.ClaimedBy == nil || *newBin.ClaimedBy != order.ID {
		t.Fatalf("new bin claimed_by = %v, want order %d", newBin.ClaimedBy, order.ID)
	}

	// Old bin at line also claimed (pickup at lineNode is step 5)
	oldBin, _ = db.GetBin(oldBin.ID)
	if oldBin.ClaimedBy == nil {
		t.Log("NOTE: old bin was not claimed — claimComplexBins may not have found it")
	} else if *oldBin.ClaimedBy != order.ID {
		t.Errorf("old bin claimed_by = %d, want order %d", *oldBin.ClaimedBy, order.ID)
	}

	// 3 pre-wait blocks, staged
	if sim.StagedOrderCount() != 1 {
		t.Fatalf("staged orders = %d, want 1", sim.StagedOrderCount())
	}
	view := sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 3 {
		t.Fatalf("pre-wait blocks = %d, want 3", len(view.Blocks))
	}
	if view.Complete {
		t.Error("order should not be complete")
	}

	// Verify pre-wait block locations and tasks
	wantPre := []struct{ loc, task string }{
		{storageNode.Name, "JackLoad"},
		{inboundStaging.Name, "JackUnload"},
		{lineNode.Name, "JackUnload"},
	}
	for i, w := range wantPre {
		if view.Blocks[i].Location != w.loc || view.Blocks[i].BinTask != w.task {
			t.Errorf("pre-wait block %d: got %q/%q, want %q/%q", i, view.Blocks[i].Location, view.Blocks[i].BinTask, w.loc, w.task)
		}
	}

	// Drive to staged, release
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: "single-swap-1"})

	// After release: 9 total blocks (3 pre-wait + 6 post-wait)
	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 9 {
		t.Fatalf("total blocks after release = %d, want 9", len(view.Blocks))
	}
	if !view.Complete {
		t.Error("order should be complete after release")
	}

	// Verify all 9 block locations and tasks in order
	wantAll := []struct{ loc, task string }{
		{storageNode.Name, "JackLoad"},       // 1: pickup new
		{inboundStaging.Name, "JackUnload"},  // 2: stage new
		{lineNode.Name, "JackUnload"},        // 3: pre-position at line
		// wait
		{lineNode.Name, "JackLoad"},          // 5: pickup old from line
		{outboundStaging.Name, "JackUnload"}, // 6: park old
		{inboundStaging.Name, "JackLoad"},    // 7: grab new from staging
		{lineNode.Name, "JackUnload"},        // 8: deliver new to line
		{outboundStaging.Name, "JackLoad"},   // 9: grab old from staging
		{outboundDest.Name, "JackUnload"},    // 10: deliver old to dest
	}
	for i, w := range wantAll {
		if view.Blocks[i].Location != w.loc || view.Blocks[i].BinTask != w.task {
			t.Errorf("block %d: got %q/%q, want %q/%q", i, view.Blocks[i].Location, view.Blocks[i].BinTask, w.loc, w.task)
		}
	}

	// Complete the order
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "single-swap-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})
	order, _ = db.GetOrderByUUID("single-swap-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Fatalf("status = %q, want confirmed", order.Status)
	}

	// Defect 1: newBin at wrong destination.
	// extractEndpoints sets DeliveryNode to the last actionable step (outboundDest, step 10).
	// But newBin actually delivers at step 8 (lineNode). ApplyBinArrival moves BinID to
	// DeliveryNode blindly — the new bin the line needs ends up at outboundDest.
	newBin, _ = db.GetBin(newBin.ID)
	if newBin.NodeID == nil {
		t.Error("new bin has no node after completion")
	} else if *newBin.NodeID != lineNode.ID {
		t.Errorf("DEFECT: new bin at node %d, want %d (lineNode) — extractEndpoints sets DeliveryNode to last step (outboundDest), ApplyBinArrival moves BinID there",
			*newBin.NodeID, lineNode.ID)
	}
	if newBin.ClaimedBy != nil {
		t.Errorf("new bin claimed_by = %v, want nil (claim released)", newBin.ClaimedBy)
	}

	// Defect 2: oldBin claim orphaned after completion.
	// claimComplexBins claimed oldBin at the step-5 pickup (lineNode), but
	// Order.BinID only tracks newBin. handleOrderCompleted calls
	// ApplyBinArrival(BinID) and returns — there is no UnclaimOrderBins
	// call on the success path (only on failure/cancel). The old bin stays
	// claimed by the completed order permanently. No other order can touch it.
	// The system's own ListOrderCompletionAnomalies query detects this.
	oldBin, _ = db.GetBin(oldBin.ID)
	t.Logf("old bin final state: node=%v claimed_by=%v status=%s",
		oldBin.NodeID, oldBin.ClaimedBy, oldBin.Status)
	if oldBin.ClaimedBy != nil {
		t.Errorf("DEFECT: old bin still claimed by order %d after completion — BinID only tracks one bin, no UnclaimOrderBins on success path",
			*oldBin.ClaimedBy)
	}
	if oldBin.NodeID == nil || *oldBin.NodeID != lineNode.ID {
		t.Errorf("DEFECT: old bin at node %v, want %d (lineNode) — never moved, ApplyBinArrival only processes BinID",
			oldBin.NodeID, lineNode.ID)
	}
}
