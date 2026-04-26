//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// Engine-level regression tests.
//
// Each TestRegression_* captures a specific bug that previously
// escaped — the test was added alongside the fix and stays here as
// a permanent guard. Kept in its own file so the suite is easy to
// browse for a reviewer looking for "which bugs do we have
// regression coverage for" without wading through the happy-path
// behavior tests in engine_test.go.

// --- Regression: Bug 1+2 — Bin moves to destination on DELIVERED, not CONFIRMED ---
// Verifies that after fleet reports FINISHED (order transitions to delivered),
// the bin's node_id is already at the delivery node — NOT still at source.
// Prior to fix, bin only moved on confirmed (after Edge round-trip), leaving
// telemetry stale during the delivery→confirmation window.
func TestRegression_BinMovesOnDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REGR-DELIVER")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "regr-deliver-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "regr-deliver-1")

	// Drive to FINISHED — fleet physically delivered the bin
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "regr-deliver-1", "delivered")

	// KEY ASSERTION: bin should already be at the line node BEFORE confirmation
	testdb.AssertBinAtNode(t, db, *order.BinID, lineNode.ID)
	testdb.AssertBinUnclaimed(t, db, *order.BinID)

	// Confirmation should still work (idempotent — bin already there)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "regr-deliver-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "regr-deliver-1", "confirmed")

	// Bin still at line node after confirmation
	testdb.AssertBinAtNode(t, db, *order.BinID, lineNode.ID)
}

// TestRegression_OrderFailedNotifiesEdge regression-tests the fix that adds
// sendToEdge(TypeOrderError) to the EventOrderFailed handler in wiring.go.
//
// Before the fix, the EventOrderFailed handler only logged + audited + called
// maybeCreateReturnOrder. It never sent a message to Edge. Fleet-driven
// failures, scanner-driven failures, and compound-parent failures all flowed
// through this handler — so Edge never learned about any of them, and the
// operator's UI showed orders as still active even after Core had marked them
// failed.
//
// The fix mirrors the EventOrderCancelled handler's pattern: when StationID
// and EdgeUUID are populated, send a TypeOrderError envelope to the outbox.
func TestRegression_OrderFailedNotifiesEdge(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Emit EventOrderFailed with populated fields directly (skips needing
	// a real fleet failure to drive the test).
	eng.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID:   42,
		EdgeUUID:  "regr-fail-edge-uuid",
		StationID: "line-1",
		ErrorCode: "fleet_failed",
		Detail:    "rds HTTP 400: simulated fleet failure",
	}})

	// Assert: an order.error envelope landed in the outbox addressed to line-1
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM outbox
		WHERE msg_type = 'order.error' AND station_id = $1
	`, "line-1").Scan(&count)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if count == 0 {
		t.Errorf("expected order.error in outbox for line-1 after EventOrderFailed; got 0 rows — " +
			"EventOrderFailed handler is not pushing failure notifications to Edge")
	}
}

// TestRegression_OrderFailedSkipsEmptyEdgeUUID asserts that the EventOrderFailed
// handler's notification gate skips orders with empty EdgeUUID — auto-return
// orders are Core-internal and have no Edge counterpart to notify.
func TestRegression_OrderFailedSkipsEmptyEdgeUUID(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	eng.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{
		OrderID:   99,
		EdgeUUID:  "", // intentionally empty (auto-return-style internal order)
		StationID: "line-1",
		ErrorCode: "structural",
		Detail:    "no source bin",
	}})

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'order.error'`).Scan(&count)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if count != 0 {
		t.Errorf("outbox has %d order.error messages, want 0 — empty EdgeUUID should skip Edge notification", count)
	}
}

// --- Regression: Bug 4 — Cancel with empty EdgeUUID does not notify Edge ---
// Auto-return orders created by Core have no EdgeUUID and are never dispatched
// to the fleet, so cancellation is engine-internal (recovery/timeout) and
// emits EventOrderCancelled directly — not through handleVendorStatusChange.
// The guard at wiring.go:98 prevents sendToEdge with an empty UUID.
func TestRegression_CancelEmptyEdgeUUID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create a real bin for the auto-return order to reference
	bin := createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-REGR-CANCEL")

	// Create an order with empty EdgeUUID (simulates auto-return order)
	autoReturn := &orders.Order{
		EdgeUUID:     "",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeStore,
		Status:       dispatch.StatusPending,
		SourceNode:   lineNode.Name,
		DeliveryNode: storageNode.Name,
		BinID:        &bin.ID,
		PayloadDesc:  "auto_return",
	}
	if err := db.CreateOrder(autoReturn); err != nil {
		t.Fatalf("create auto-return order: %v", err)
	}

	// Cancel via event — should NOT send to Edge or panic
	eng.Events.Emit(Event{Type: EventOrderCancelled, Payload: OrderCancelledEvent{
		OrderID:        autoReturn.ID,
		EdgeUUID:       "",
		StationID:      "line-1",
		Reason:         "test cancel",
		PreviousStatus: dispatch.StatusPending,
	}})

	// Assertion 1: No cancel message in outbox (Edge was NOT notified)
	var outboxCount int
	err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'order_cancelled'`).Scan(&outboxCount)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if outboxCount != 0 {
		t.Errorf("outbox has %d order_cancelled messages, want 0 — empty EdgeUUID should skip Edge notification", outboxCount)
	}

	// Assertion 2: No auto-return order was created (payload_desc=auto_return prevents loops,
	// but verify it didn't slip through)
	var returnCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM orders WHERE payload_desc = 'auto_return' AND source_node = $1`, storageNode.Name).Scan(&returnCount)
	if err != nil {
		t.Fatalf("query return orders: %v", err)
	}
	if returnCount != 0 {
		t.Errorf("auto-return order was created for an already-return order — loop guard may be broken")
	}
}

// --- Regression: Multi-bin order moves ALL bins on DELIVERED ---
// Verifies that when a complex order has multiple claimed bins (order_bins junction
// rows), ALL bins move to their destinations on fleet FINISHED — not just one.
// The single-bin path is already covered by TestRegression_BinMovesOnDelivered.
func TestRegression_MultiBinMovesOnDelivered(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, inboundStaging, outboundStaging, outboundDest, bp := setupProductionNodes(t, db)

	// Two bins: new material at storage, old material at line
	newBin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REGR-MB-NEW")
	oldBin := createTestBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-REGR-MB-OLD")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "regr-multibin-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: inboundStaging.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "wait"},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: outboundStaging.Name},
			{Action: "pickup", Node: inboundStaging.Name},
			{Action: "dropoff", Node: lineNode.Name},
			{Action: "pickup", Node: outboundStaging.Name},
			{Action: "dropoff", Node: outboundDest.Name},
		},
	})

	order := testdb.RequireOrder(t, db, "regr-multibin-1")

	// Verify junction table was populated (multi-bin path)
	orderBins, err := db.ListOrderBins(order.ID)
	if err != nil {
		t.Fatalf("list order bins: %v", err)
	}
	if len(orderBins) < 2 {
		t.Fatalf("expected >= 2 order_bins rows, got %d", len(orderBins))
	}

	// Drive to FINISHED — fleet physically delivered
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "regr-multibin-1", "delivered")

	// KEY ASSERTION: both bins should have moved to their resolved destinations.
	// Step simulation: newBin (storage→inbound→line), oldBin (line→outbound-staging→outbound-dest)
	// Final destinations: newBin → lineNode, oldBin → outboundDest.
	testdb.RequireBinAtNode(t, db, newBin.ID, lineNode.ID)
	testdb.RequireBinAtNode(t, db, oldBin.ID, outboundDest.ID)

	// Both bins should be unclaimed after delivery
	testdb.AssertBinUnclaimed(t, db, newBin.ID)
	testdb.AssertBinUnclaimed(t, db, oldBin.ID)

	// Confirmation should work (idempotent — bins already at destinations)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "regr-multibin-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "regr-multibin-1", "confirmed")

	// Verify junction table was cleaned up on completion
	orderBinsAfter, _ := db.ListOrderBins(order.ID)
	if len(orderBinsAfter) != 0 {
		t.Errorf("order_bins rows after confirmation = %d, want 0 (should be cleaned up)", len(orderBinsAfter))
	}
}

// --- Regression: handleOrderCompleted is idempotent for single-bin ---
// Verifies that calling handleOrderCompleted (confirmation) after bins already
// moved on delivery does NOT move them again or cause errors.
func TestRegression_CompletionIdempotentAfterDelivery(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REGR-IDEMP")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "regr-idemp-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrder(t, db, "regr-idemp-1")

	// Drive to FINISHED — bin moves to line node
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	binAfterDelivery := testdb.RequireBin(t, db, *order.BinID)

	// Record the bin state after delivery — confirmation must not change it
	nodeAfterDelivery := *binAfterDelivery.NodeID

	// Confirm — handleOrderCompleted runs but should be idempotent
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "regr-idemp-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	binAfterConfirm := testdb.RequireBin(t, db, *order.BinID)

	// Bin should still be at the same node — no double-move
	if binAfterConfirm.NodeID == nil || *binAfterConfirm.NodeID != nodeAfterDelivery {
		t.Errorf("bin moved during confirmation: was at %d, now at %v — completion should be idempotent",
			nodeAfterDelivery, binAfterConfirm.NodeID)
	}
	if binAfterConfirm.ClaimedBy != nil {
		t.Errorf("bin still claimed after confirmation: %v", binAfterConfirm.ClaimedBy)
	}
}
