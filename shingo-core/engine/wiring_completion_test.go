package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// --- Characterization tests for handleOrderCompleted (wiring.go:410-495) ---
//
// handleOrderCompleted is the safety-net arrival path triggered by Edge receipt
// confirmation (HandleOrderReceipt → EventOrderCompleted). The bin should
// normally already be at its destination (moved by handleOrderDelivered at
// FINISHED), so the primary job is idempotency. These tests characterize
// each branch.

// deliveredOrder creates fixtures, dispatches a retrieve order, and drives it
// through RUNNING → FINISHED so it's in "delivered" status ready for receipt.
func deliveredOrder(t *testing.T) (db *store.DB, eng *Engine, sim *simulator.SimulatorBackend, d *dispatch.Dispatcher, order *store.Order, lineNode *store.Node) {
	t.Helper()
	db = testDB(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CO-1")

	sim = simulator.New()
	eng = newTestEngine(t, db, sim)
	d = eng.Dispatcher()

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "co-order-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("co-order-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Drive to delivered
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order, _ = db.GetOrderByUUID("co-order-1")
	if order.Status != "delivered" {
		t.Fatalf("expected delivered, got %q", order.Status)
	}

	return db, eng, sim, d, order, sd.LineNode
}

// TC-CO-1: Normal receipt — bin already at destination (idempotent safety net).
// handleOrderCompleted should detect bin is already at dest and skip ApplyBinArrival.
func TestOrderCompleted_BinAlreadyAtDest(t *testing.T) {
	t.Parallel()
	db, _, _, d, order, lineNode := deliveredOrder(t)

	// Verify bin was already moved by handleOrderDelivered (FINISHED)
	if order.BinID != nil {
		bin, _ := db.GetBin(*order.BinID)
		if bin.NodeID != nil && *bin.NodeID == lineNode.ID {
			t.Logf("bin already at line node before receipt — good, idempotency path will trigger")
		}
	}

	// Send receipt — triggers handleOrderCompleted
	env := testEnvelope()
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "co-order-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	// Order should be confirmed after receipt
	got, _ := db.GetOrderByUUID("co-order-1")
	if got.Status != "confirmed" {
		t.Errorf("status after receipt: got %q, want %q", got.Status, "confirmed")
	}

	// Bin should still be at line node (no double-move)
	if got.BinID != nil {
		bin, _ := db.GetBin(*got.BinID)
		if bin.NodeID == nil || *bin.NodeID != lineNode.ID {
			t.Errorf("bin should remain at line node, got node_id=%v", bin.NodeID)
		}
	}
}

// TC-CO-2: handleOrderCompleted with missing BinID — early return, no crash.
func TestOrderCompleted_NoBinID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create an order with no bin assigned
	order := &store.Order{
		EdgeUUID:     "co-no-bin",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
		// BinID is nil
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Should not panic — early return on nil BinID
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})
}

// TC-CO-3: handleOrderCompleted with missing source/delivery nodes — early return.
func TestOrderCompleted_MissingNodes(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Order with empty source and delivery nodes
	order := &store.Order{
		EdgeUUID:  "co-no-nodes",
		StationID: "line-1",
		OrderType: dispatch.OrderTypeRetrieve,
		Status:    "delivered",
		// SourceNode and DeliveryNode empty
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Should not panic — early return
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})
}

// TC-CO-4: handleOrderCompleted for non-existent order — log and return.
func TestOrderCompleted_NonExistentOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Should not panic
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   999999,
		EdgeUUID:  "ghost",
		StationID: "line-1",
	})
}

// TC-CO-5: handleOrderCompleted safety net — bin NOT at dest yet.
// Simulates the case where handleOrderDelivered failed or didn't move the bin.
// handleOrderCompleted should apply the arrival.
func TestOrderCompleted_SafetyNetArrival(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CO-SN")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create a delivered order with bin still at source (simulating delivery arrival failure).
	order := &store.Order{
		EdgeUUID:     "co-safety-net",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       "delivered",
		SourceNode:   sd.StorageNode.Name,
		DeliveryNode: sd.LineNode.Name,
		BinID:        &bin.ID,
		PayloadCode:  sd.Payload.Code,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Verify bin is still at storage node
	binBefore, _ := db.GetBin(bin.ID)
	if binBefore.NodeID == nil || *binBefore.NodeID != sd.StorageNode.ID {
		t.Fatalf("bin should be at storage node before completion")
	}

	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})

	// Bin should now be at the delivery (line) node via ApplyBinArrival
	binAfter, _ := db.GetBin(bin.ID)
	if binAfter.NodeID == nil || *binAfter.NodeID != sd.LineNode.ID {
		t.Errorf("bin node after safety-net arrival: got %v, want %d (line node)", binAfter.NodeID, sd.LineNode.ID)
	}
}

// TC-CO-6: handleOrderCompleted with retrieve_empty payload → staged=false override.
func TestOrderCompleted_RetrieveEmptyOverride(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CO-RE")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	order := &store.Order{
		EdgeUUID:     "co-retrieve-empty",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       "delivered",
		SourceNode:   sd.StorageNode.Name,
		DeliveryNode: sd.LineNode.Name,
		BinID:        &bin.ID,
		PayloadCode:  sd.Payload.Code,
		PayloadDesc:  "retrieve_empty",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})

	// Bin should be at line node with status "available" (not "staged")
	// because retrieve_empty forces staged=false.
	binAfter, _ := db.GetBin(bin.ID)
	if binAfter.NodeID == nil || *binAfter.NodeID != sd.LineNode.ID {
		t.Errorf("bin not at line node after retrieve_empty completion")
	}
	if binAfter.Status == "staged" {
		t.Error("retrieve_empty should override staging — bin should not be staged")
	}
}

// TC-CO-7: handleOrderCompleted with complex order + WaitIndex > 0 → staged=false override.
func TestOrderCompleted_ComplexWaitOverride(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CO-CW")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	order := &store.Order{
		EdgeUUID:     "co-complex-wait",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       "delivered",
		SourceNode:   sd.StorageNode.Name,
		DeliveryNode: sd.LineNode.Name,
		BinID:        &bin.ID,
		PayloadCode:  sd.Payload.Code,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// WaitIndex is not stored by CreateOrder; set it via the dedicated updater.
	if err := db.UpdateOrderWaitIndex(order.ID, 1); err != nil {
		t.Fatalf("update wait index: %v", err)
	}

	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})

	binAfter, _ := db.GetBin(bin.ID)
	if binAfter.NodeID == nil || *binAfter.NodeID != sd.LineNode.ID {
		t.Errorf("bin not at line node after complex-wait completion")
	}
	if binAfter.Status == "staged" {
		t.Error("complex order with WaitIndex > 0 should override staging")
	}
}
