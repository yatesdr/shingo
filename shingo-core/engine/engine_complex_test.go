package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
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
	t.Parallel()
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

	order := testdb.RequireOrderStatus(t, db, "cx-cancel-1", dispatch.StatusDispatched)

	// Verify bin was claimed
	bin, err := db.GetBin(bin.ID)
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
	t.Parallel()
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

	order := testdb.RequireOrder(t, db, "cx-fail-1")

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
	t.Parallel()
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

	order := testdb.RequireOrderStatus(t, db, "cx-empty-wait-1", dispatch.StatusDispatched)

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

	// Empty release completes the fleet order. Drive it through to receipt
	// so ApplyBinArrival fires and moves the bin to lineNode.
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "cx-empty-wait-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	// Verify no panic and order transitions correctly
	order, _ = db.GetOrderByUUID("cx-empty-wait-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Errorf("order status = %q, want confirmed", order.Status)
	}

	// Bin should be at lineNode (the dropoff destination), unclaimed
	bin, _ = db.GetBin(bin.ID)
	if bin.NodeID == nil || *bin.NodeID != lineNode.ID {
		t.Errorf("bin node = %v, want %d (lineNode)", bin.NodeID, lineNode.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil (claim released after completion)", bin.ClaimedBy)
	}
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

	order := testdb.RequireOrder(t, db, "cx-redir-1")

	// Drive to staged (dwelling)
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "DWELLING")

	if err := db.UpdateOrderStatus(order.ID, dispatch.StatusStaged, "dwelling"); err != nil {
		t.Fatalf("update to staged: %v", err)
	}

	// Simulate redirect: update DeliveryNode directly (PrepareRedirect does this).
	// StepsJSON still has storageNode as the last dropoff — this is the TC-48 bug surface.
	if err := db.UpdateOrderDeliveryNode(order.ID, newDest.Name); err != nil {
		t.Fatalf("update delivery node: %v", err)
	}

	// Release — HandleOrderRelease should patch the segment with the new DeliveryNode
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "cx-redir-1",
	})

	// Verify the fleet order blocks reference the new destination
	order, _ = db.GetOrderByUUID("cx-redir-1")
	if order.VendorOrderID != "" {
		view := sim.GetOrder(order.VendorOrderID)
		if view == nil {
			t.Fatal("simulator should have the post-release order")
		}
		lastBlock := view.Blocks[len(view.Blocks)-1]
		if lastBlock.Location != newDest.Name {
			t.Errorf("last fleet block location = %s, want %s — post-wait blocks not patched for redirect", lastBlock.Location, newDest.Name)
		}
	}
}

// --- Test: Ghost robot — claimComplexBins finds no bin (TC-49) ---
//
// Scenario: A complex order specifies a pickup at a node, but the node
// has no bins matching the payload (or all bins are already claimed).
// claimComplexBins returns a planningError with code "no_bin", and the
// order is failed at the planning stage — no robot is dispatched.
//
// Expected:
// 1. Order status = failed
// 2. BinID = nil
// 3. No vendor order created (no fleet interaction)
// 4. No auto-return order created
func TestComplexOrder_GhostRobotNoBin(t *testing.T) {
	t.Parallel()
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

	order := testdb.RequireOrder(t, db, "cx-ghost-1")

	// Order should fail at planning — no bin available
	if order.Status != dispatch.StatusFailed {
		t.Fatalf("status = %q, want failed (no bin at pickup should fail at planning)", order.Status)
	}
	if order.BinID != nil {
		t.Errorf("expected BinID=nil (no bin at pickup), got %d", *order.BinID)
	}

	// No vendor order should be created — robot never dispatched
	if order.VendorOrderID != "" {
		t.Errorf("expected no vendor order, got %q — ghost robot was dispatched", order.VendorOrderID)
	}

	// No auto-return should be created (BinID=nil)
	allOrders, _ := db.ListOrders("", 50)
	for _, o := range allOrders {
		if o.PayloadDesc == "auto_return" {
			t.Errorf("auto-return order created for failed planning (no bin) — order %d", o.ID)
		}
	}
}

// --- Test: Concurrent complex orders targeting same node — double claim race (TC-50) ---
//
// Scenario: Two complex orders are submitted sequentially, both picking up
// from the same storage node that has only one available bin.
// claimComplexBins runs for both orders in sequence. The first should claim
// the bin; the second should fail at planning with "no_bin".
//
// Expected: First order claims the bin and dispatches. Second order fails
// at planning — no ghost robot. No double-claim occurs.
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

	// First order should have claimed the bin and dispatched
	if order1.BinID == nil {
		t.Fatalf("order1 should have claimed the bin")
	}
	if order1.Status != dispatch.StatusDispatched {
		t.Errorf("order1 status = %q, want dispatched", order1.Status)
	}

	// Second order should fail at planning — no bin available
	if order2.Status != dispatch.StatusFailed {
		t.Errorf("order2 status = %q, want failed (no bin available after first order claimed it)", order2.Status)
	}
	if order2.BinID != nil {
		t.Errorf("order2 should have BinID=nil, got %d", *order2.BinID)
	}

	// No double-claim — bin belongs to order1 only
	bin, _ = db.GetBin(bin.ID)
	if bin.ClaimedBy == nil || *bin.ClaimedBy != order1.ID {
		t.Errorf("bin claimed by %v, want order %d", bin.ClaimedBy, order1.ID)
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
	t.Parallel()
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

	order := testdb.RequireOrderStatus(t, db, "seq-backfill-1", dispatch.StatusDispatched)

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
//	wait(CoreNode) → pickup(CoreNode) → dropoff(OutboundDest)
//
// Robot drives to line and holds (RDS BinTask=Wait), operator releases, picks
// up old bin, delivers to outbound destination. claimComplexBins iterates ALL
// steps (including post-wait) and finds the pickup(lineNode) step, claiming
// the bin there.
//
// Expected: 1 pre-wait block (Wait), staged order. After release: 3 total blocks.
// After completion: bin at outbound dest.
func TestComplexOrder_SequentialRemoval(t *testing.T) {
	t.Parallel()
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
			{Action: "wait", Node: lineNode.Name},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: outboundDest.Name},
		},
	})

	order := testdb.RequireOrderStatus(t, db, "seq-removal-1", dispatch.StatusDispatched)

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
	if view.Blocks[0].Location != lineNode.Name || view.Blocks[0].BinTask != "Wait" {
		t.Errorf("block 0: location=%q task=%q, want %q/Wait", view.Blocks[0].Location, view.Blocks[0].BinTask, lineNode.Name)
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

	// Drive to confirmed — full lifecycle including receipt
	order = driveToConfirmed(t, sim, d, db, "seq-removal-1")
	if order.Status != dispatch.StatusConfirmed {
		t.Fatalf("after receipt: status = %q, want confirmed", order.Status)
	}

	// Bin moved to outboundDest (single pickup — extractEndpoints correct)
	bin, _ := db.GetBin(*order.BinID)
	if bin.NodeID == nil || *bin.NodeID != outboundDest.ID {
		t.Errorf("bin node = %v, want %d (outboundDest)", bin.NodeID, outboundDest.ID)
	}
	if bin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v, want nil (claim released)", bin.ClaimedBy)
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
	t.Parallel()
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

	order := testdb.RequireOrderStatus(t, db, "tworobot-resupply-1", dispatch.StatusDispatched)

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
//	wait(CoreNode) → pickup(CoreNode) → dropoff(OutboundDest)
//
// Same structure as sequential removal but in the two-robot context.
// Robot drives to node and holds (RDS BinTask=Wait). claimComplexBins
// finds the post-wait pickup and claims the bin.
//
// Expected: 1 pre-wait block (Wait), staged. After release: 3 blocks.
// After completion: bin at outbound dest.
func TestComplexOrder_TwoRobotSwap_Removal(t *testing.T) {
	t.Parallel()
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
			{Action: "wait", Node: lineNode.Name},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: outboundDest.Name},
		},
	})

	order := testdb.RequireOrderStatus(t, db, "tworobot-removal-1", dispatch.StatusDispatched)

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
	if view.Blocks[0].Location != lineNode.Name || view.Blocks[0].BinTask != "Wait" {
		t.Errorf("block 0: location=%q task=%q, want %q/Wait", view.Blocks[0].Location, view.Blocks[0].BinTask, lineNode.Name)
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

	// Block structure: line/Wait, line/JackLoad, dest/JackUnload
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
	t.Parallel()
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

	stageOrder := testdb.RequireOrderStatus(t, db, "stage-1", dispatch.StatusDispatched)
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

	deliverOrder := testdb.RequireOrderStatus(t, db, "deliver-1", dispatch.StatusDispatched)

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

// --- TC-60: Single-Robot 9-Step Swap ---
//
// Pattern from BuildSingleSwapSteps — the most complex production pattern:
//
//	pickup(source) → dropoff(inStaging) → wait(line)
//	→ pickup(line) → dropoff(outStaging) → pickup(inStaging)
//	→ dropoff(line) → pickup(outStaging) → dropoff(outDest)
//
// One robot swaps two bins in a single trip. It stages the new bin at inbound
// staging, drives to line and holds (RDS BinTask=Wait), operator releases,
// swaps old for new, then delivers the old bin to outbound destination.
//
// This test validates the order_bins junction table fix for multi-bin
// complex orders. Two bins are tracked:
//   - newBin: storage → inStaging → lineNode (final dest, step 7)
//   - oldBin: lineNode → outStaging → outboundDest (final dest, step 9)
//
// The fix:
//   - claimComplexBins populates order_bins with per-bin destinations computed
//     by resolvePerBinDestinations (bin flow simulation through the step list)
//   - handleMultiBinCompleted moves each bin to its per-step destination via
//     ApplyMultiBinArrival (single atomic transaction)
//   - Both bins are unclaimed on the success path
//
// Previously failed (OPEN defect) because Order.BinID is *int64 (single bin)
// and handleOrderCompleted only processed one bin via ApplyBinArrival.
//
// Expected: 3 pre-wait blocks, staged order. After release: 9 total blocks.
// Both bins at correct destinations, both unclaimed.
func TestComplexOrder_SingleRobotSwap(t *testing.T) {
	t.Parallel()
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
			{Action: "wait", Node: lineNode.Name},            // 3 drive to node + hold
			{Action: "pickup", Node: lineNode.Name},          // 4
			{Action: "dropoff", Node: outboundStaging.Name},  // 5
			{Action: "pickup", Node: inboundStaging.Name},    // 6
			{Action: "dropoff", Node: lineNode.Name},         // 7
			{Action: "pickup", Node: outboundStaging.Name},   // 8
			{Action: "dropoff", Node: outboundDest.Name},     // 9
		},
	})

	order := testdb.RequireOrderStatus(t, db, "single-swap-1", dispatch.StatusDispatched)

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
		{lineNode.Name, "Wait"},
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
		{lineNode.Name, "Wait"},              // 3: drive to node + hold
		// (release)
		{lineNode.Name, "JackLoad"},          // 4: pickup old from line
		{outboundStaging.Name, "JackUnload"}, // 5: park old
		{inboundStaging.Name, "JackLoad"},    // 6: grab new from staging
		{lineNode.Name, "JackUnload"},        // 7: deliver new to line
		{outboundStaging.Name, "JackLoad"},   // 8: grab old from staging
		{outboundDest.Name, "JackUnload"},    // 9: deliver old to dest
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

	// Fixed Defect 1: newBin should be at lineNode (step 8 destination).
	// The order_bins junction table records per-bin destinations computed by
	// resolvePerBinDestinations. handleMultiBinCompleted moves each bin to
	// its correct destination instead of using Order.DeliveryNode.
	newBin, _ = db.GetBin(newBin.ID)
	if newBin.NodeID == nil {
		t.Error("new bin has no node after completion")
	} else if *newBin.NodeID != lineNode.ID {
		t.Errorf("new bin at node %d, want %d (lineNode) — per-bin destination from order_bins junction table",
			*newBin.NodeID, lineNode.ID)
	}
	if newBin.ClaimedBy != nil {
		t.Errorf("new bin claimed_by = %v, want nil (claim released by ApplyMultiBinArrival)", newBin.ClaimedBy)
	}

	// Fixed Defect 3: newBin at lineside must be "available", not "staged".
	// resolveNodeStaging used to mark ALL lineside bins as staged, but for
	// operator-released swap orders (WaitIndex > 0) the operator already
	// confirmed by releasing the wait. Staging would leave bins permanently
	// stuck with no unstage mechanism.
	if newBin.Status != "available" {
		t.Errorf("new bin status = %q, want available (operator confirmed by releasing wait)", newBin.Status)
	}

	// Fixed Defect 2: oldBin should be at outboundDest (step 10 destination)
	// and unclaimed. The junction table tracks both bins; ApplyMultiBinArrival
	// moves all bins and unclaims them in one transaction.
	oldBin, _ = db.GetBin(oldBin.ID)
	t.Logf("old bin final state: node=%v claimed_by=%v status=%s",
		oldBin.NodeID, oldBin.ClaimedBy, oldBin.Status)
	if oldBin.ClaimedBy != nil {
		t.Errorf("old bin still claimed by order %d after completion — expected unclaimed",
			*oldBin.ClaimedBy)
	}
	if oldBin.NodeID == nil || *oldBin.NodeID != outboundDest.ID {
		t.Errorf("old bin at node %v, want %d (outboundDest) — per-bin destination from order_bins junction table",
			oldBin.NodeID, outboundDest.ID)
	}
	if oldBin.Status != "available" {
		t.Errorf("old bin status = %q, want available (swap order with released wait)", oldBin.Status)
	}
}

// =============================================================================
// TC-DW: Double-wait complex order — Phase 3 evacuate flow prerequisite
//
// This test verifies that a complex order with TWO wait steps is handled
// correctly by the Core dispatcher. The evacuate changeover flow requires:
//
//   pickup(Storage) → dropoff(Line) → wait₁ → pickup(Line) → dropoff(Storage) → wait₂ → pickup(Storage) → dropoff(Line)
//
// Expected lifecycle:
//   1. Dispatch: 2 pre-wait blocks (pickup Storage, dropoff Line), order staged
//   2. Drive RUNNING → WAITING: status "staged"
//   3. First release: appends 2 blocks (pickup Line, dropoff Storage), order
//      remains staged (second wait still ahead)
//   4. Drive RUNNING → WAITING: status "staged" again
//   5. Second release: appends 2 blocks (pickup Storage, dropoff Line), order
//      marked complete
//   6. Drive RUNNING → FINISHED: order "delivered"
//
// If this test FAILS, the dispatcher's wait-splitting logic only handles a
// single wait and Phase 3 evacuate orders cannot ship safely.
// =============================================================================

func TestComplexOrder_DoubleWait(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Need bins at both nodes for claimComplexBins:
	//   - step 0: pickup(storage) → needs bin at storage
	//   - step 3: pickup(line) → needs bin at line
	//   - step 6: pickup(storage) → needs second bin at storage
	_ = createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-DW-S1")
	_ = createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-DW-S2")
	_ = createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-DW-L1")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	// Submit double-wait complex order:
	//   pickup(storage) → dropoff(line) → wait → pickup(line) → dropoff(storage) → wait → pickup(storage) → dropoff(line)
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "double-wait-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: storageNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order := testdb.RequireOrderStatus(t, db, "double-wait-1", dispatch.StatusDispatched)

	// ── Phase 1: Pre-wait blocks ──────────────────────────────────────────
	// Only blocks before the first wait should be sent to the fleet.
	if sim.StagedOrderCount() != 1 {
		t.Fatalf("staged orders = %d, want 1", sim.StagedOrderCount())
	}
	view := sim.GetOrder(order.VendorOrderID)
	if view == nil {
		t.Fatal("simulator should have the order")
	}
	if len(view.Blocks) != 2 {
		t.Fatalf("pre-wait blocks = %d, want 2 (pickup + dropoff before wait₁)", len(view.Blocks))
	}
	if view.Complete {
		t.Fatal("order should NOT be complete (has 2 wait steps remaining)")
	}
	if view.Blocks[0].Location != storageNode.Name || view.Blocks[0].BinTask != "JackLoad" {
		t.Errorf("block 0: got %s/%s, want %s/JackLoad", view.Blocks[0].Location, view.Blocks[0].BinTask, storageNode.Name)
	}
	if view.Blocks[1].Location != lineNode.Name || view.Blocks[1].BinTask != "JackUnload" {
		t.Errorf("block 1: got %s/%s, want %s/JackUnload", view.Blocks[1].Location, view.Blocks[1].BinTask, lineNode.Name)
	}

	// ── Phase 2: Drive to first WAITING ───────────────────────────────────
	sim.DriveState(order.VendorOrderID, "RUNNING")
	order, _ = db.GetOrderByUUID("double-wait-1")
	if order.Status != dispatch.StatusInTransit {
		t.Fatalf("after RUNNING: status = %q, want %q", order.Status, dispatch.StatusInTransit)
	}

	sim.DriveState(order.VendorOrderID, "WAITING")
	order, _ = db.GetOrderByUUID("double-wait-1")
	if order.Status != dispatch.StatusStaged {
		t.Fatalf("after first WAITING: status = %q, want %q", order.Status, dispatch.StatusStaged)
	}

	// ── Phase 3: First release (wait₁) ───────────────────────────────────
	// Should append ONLY the 2 blocks between wait₁ and wait₂.
	// The order must remain staged (not complete) because wait₂ is still ahead.
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "double-wait-1",
	})

	view = sim.GetOrder(order.VendorOrderID)

	// CRITICAL ASSERTION: After first release, we expect 4 blocks total
	// (2 pre-wait₁ + 2 between wait₁ and wait₂), NOT 6 (all blocks).
	// If this fails with 6 blocks, splitPostWait is dumping all remaining
	// steps and the second wait is never honored.
	if len(view.Blocks) != 4 {
		t.Fatalf("BUG: blocks after first release = %d, want 4 (2 pre-wait + 2 mid-wait)\n"+
			"If you see 6 blocks, splitPostWait returns ALL steps after the first\n"+
			"wait and stepsToBlocks skips the second wait action, producing 4\n"+
			"post-wait blocks instead of 2. The second wait is never honored.\n"+
			"Fix: splitPostWait must stop at the next wait, and ReleaseOrder\n"+
			"must support partial release (complete=false when more waits remain).",
			len(view.Blocks))
	}
	if view.Complete {
		t.Fatal("BUG: order marked complete after first release — second wait not honored.\n" +
			"ReleaseOrder always sets complete=true. For double-wait, the first\n" +
			"release must keep complete=false so the robot can enter WAITING again.")
	}

	// After first release, the order status should transition but must return
	// to a releasable state when the robot hits the second wait.
	order, _ = db.GetOrderByUUID("double-wait-1")
	if order.Status != dispatch.StatusInTransit {
		t.Fatalf("after first release: status = %q, want %q", order.Status, dispatch.StatusInTransit)
	}

	// ── Phase 4: Drive to second WAITING ──────────────────────────────────
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "WAITING")

	order, _ = db.GetOrderByUUID("double-wait-1")
	if order.Status != dispatch.StatusStaged {
		t.Fatalf("BUG: after second WAITING: status = %q, want %q\n"+
			"The dispatcher set in_transit after first release and the robot\n"+
			"never enters WAITING again because ReleaseOrder already sent all\n"+
			"blocks. If status is still in_transit, the fleet ran straight\n"+
			"through without stopping.",
			order.Status, dispatch.StatusStaged)
	}

	// ── Phase 5: Second release (wait₂) ──────────────────────────────────
	// Should append the final 2 blocks and mark the order complete.
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "double-wait-1",
	})

	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 6 {
		t.Fatalf("blocks after second release = %d, want 6 (2+2+2)", len(view.Blocks))
	}
	if !view.Complete {
		t.Fatal("order should be complete after second release — no more waits")
	}

	// Verify block sequence: each pair is (pickup, dropoff) at alternating nodes
	expectedBlocks := []struct {
		location string
		binTask  string
	}{
		{storageNode.Name, "JackLoad"},
		{lineNode.Name, "JackUnload"},
		{lineNode.Name, "JackLoad"},
		{storageNode.Name, "JackUnload"},
		{storageNode.Name, "JackLoad"},
		{lineNode.Name, "JackUnload"},
	}
	for i, exp := range expectedBlocks {
		if i >= len(view.Blocks) {
			break
		}
		if view.Blocks[i].Location != exp.location || view.Blocks[i].BinTask != exp.binTask {
			t.Errorf("block %d: got %s/%s, want %s/%s",
				i, view.Blocks[i].Location, view.Blocks[i].BinTask, exp.location, exp.binTask)
		}
	}

	// ── Phase 6: Drive to completion ──────────────────────────────────────
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order, _ = db.GetOrderByUUID("double-wait-1")
	if order.Status != dispatch.StatusDelivered {
		t.Fatalf("after FINISHED: status = %q, want %q", order.Status, dispatch.StatusDelivered)
	}
	t.Log("double-wait complex order completed successfully — Phase 3 evacuate flow is unblocked")
}
