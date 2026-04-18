//go:build docker

package engine

import (
	"sync"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// --- Characterization tests for handleVendorStatusChange (wiring.go:179-262) ---
//
// handleVendorStatusChange is called via EventBus when the fleet backend
// fires a status update. These tests drive state through the simulator,
// which fires events synchronously, matching production behavior.

// dispatchRetrieveOrder is a test helper that creates standard fixtures and
// dispatches a retrieve order. Returns everything needed for vendor status tests.
func dispatchRetrieveOrder(t *testing.T) (db *store.DB, eng *Engine, sim *simulator.SimulatorBackend, order *store.Order, storageNode, lineNode *store.Node) {
	t.Helper()
	db = testDB(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-VS-1")

	sim = simulator.New()
	eng = newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "vs-order-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name,
		Quantity:     1,
	})

	order = testdb.RequireOrderStatus(t, db, "vs-order-1", dispatch.StatusDispatched)
	return db, eng, sim, order, sd.StorageNode, sd.LineNode
}

// TC-VS-1: RUNNING state updates order to in_transit and assigns robot ID.
// Updated to use DriveStateWithRobot for full robot ID coverage.
func TestVendorStatus_RunningUpdatesStatus(t *testing.T) {
	t.Parallel()
	db, _, sim, order, _, _ := dispatchRetrieveOrder(t)

	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")

	got := testdb.RequireOrderStatus(t, db, "vs-order-1", "in_transit")
	if got.RobotID != "AMB-01" {
		t.Errorf("robot_id after RUNNING: got %q, want %q", got.RobotID, "AMB-01")
	}
}

// TC-VS-2: Idempotent status — driving same state twice doesn't error.
func TestVendorStatus_IdempotentStatus(t *testing.T) {
	t.Parallel()
	db, _, sim, order, _, _ := dispatchRetrieveOrder(t)

	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "RUNNING")

	testdb.AssertOrderStatus(t, db, "vs-order-1", "in_transit")
}

// TC-VS-3: FINISHED terminal state → order delivered, bin moved to dest.
func TestVendorStatus_FinishedDelivers(t *testing.T) {
	t.Parallel()
	db, _, sim, order, _, lineNode := dispatchRetrieveOrder(t)

	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	got := testdb.AssertOrderStatus(t, db, "vs-order-1", "delivered")

	// handleOrderDelivered should move the bin to the delivery node.
	if got.BinID != nil {
		testdb.AssertBinAtNode(t, db, *got.BinID, lineNode.ID)
	}
}

// TC-VS-4: FAILED terminal state → order failed, EventOrderFailed emitted.
func TestVendorStatus_FailedTerminal(t *testing.T) {
	t.Parallel()
	db, eng, sim, order, _, _ := dispatchRetrieveOrder(t)

	// Subscribe to capture the failed event.
	var failedEvt *OrderFailedEvent
	var mu sync.Mutex
	eng.Events.SubscribeTypes(func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if payload, ok := e.Payload.(OrderFailedEvent); ok {
			failedEvt = &payload
		}
	}, EventOrderFailed)

	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FAILED")

	testdb.AssertOrderStatus(t, db, "vs-order-1", "failed")

	mu.Lock()
	defer mu.Unlock()
	if failedEvt == nil {
		t.Fatal("expected EventOrderFailed to be emitted")
	}
	if failedEvt.OrderID != order.ID {
		t.Errorf("failed event order ID: got %d, want %d", failedEvt.OrderID, order.ID)
	}
	if failedEvt.ErrorCode != "fleet_failed" {
		t.Errorf("failed event error code: got %q, want %q", failedEvt.ErrorCode, "fleet_failed")
	}
}

// TC-VS-5: STOPPED terminal state → order cancelled, EventOrderCancelled emitted
// with PreviousStatus captured before the status update.
func TestVendorStatus_StoppedCancels(t *testing.T) {
	t.Parallel()
	db, eng, sim, order, _, _ := dispatchRetrieveOrder(t)

	var cancelledEvt *OrderCancelledEvent
	var mu sync.Mutex
	eng.Events.SubscribeTypes(func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if payload, ok := e.Payload.(OrderCancelledEvent); ok {
			cancelledEvt = &payload
		}
	}, EventOrderCancelled)

	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "STOPPED")

	testdb.AssertOrderStatus(t, db, "vs-order-1", "cancelled")

	mu.Lock()
	defer mu.Unlock()
	if cancelledEvt == nil {
		t.Fatal("expected EventOrderCancelled to be emitted")
	}
	if cancelledEvt.OrderID != order.ID {
		t.Errorf("cancelled event order ID: got %d, want %d", cancelledEvt.OrderID, order.ID)
	}
	// PreviousStatus should be the status before the cancelled update was applied.
	// The function captures order.Status at the top (after GetOrder), which should be
	// "in_transit" (from RUNNING). But the status UPDATE runs before the terminal block,
	// so order.Status was read before the update. Let's characterize the actual value.
	if cancelledEvt.PreviousStatus == "" {
		t.Error("cancelled event should have non-empty PreviousStatus")
	}
	t.Logf("PreviousStatus captured: %q (characterization)", cancelledEvt.PreviousStatus)
}

// TC-VS-6: Non-existent order — handleVendorStatusChange logs and returns gracefully.
func TestVendorStatus_NonExistentOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Should not panic.
	eng.handleVendorStatusChange(OrderStatusChangedEvent{
		OrderID:   999999,
		NewStatus: "RUNNING",
	})
}
