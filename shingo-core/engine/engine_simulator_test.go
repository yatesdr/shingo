//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// --- TC-15: Full Lifecycle ---
// Scenario: verifies the complete order lifecycle works end-to-end.
// Dispatches a retrieve order, drives RUNNING → FINISHED, simulates Edge receipt
// confirmation. Verifies complete lifecycle: bin moved + claim released.
func TestSimulator_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-LC")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	// Step 1: Create order
	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "lc-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrderStatus(t, db, "lc-1", dispatch.StatusDispatched)

	// Step 2: Drive RUNNING — event fires, handleVendorStatusChange updates DB
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order = testdb.RequireOrderStatus(t, db, "lc-1", "in_transit")

	// Step 3: Drive FINISHED — handleVendorStatusChange calls handleOrderDelivered
	sim.DriveState(order.VendorOrderID, "FINISHED")

	// Delivered (the call asserts the status; order is refetched at the confirmed step).
	testdb.RequireOrderStatus(t, db, "lc-1", "delivered")

	// Step 4: Simulate Edge receipt — triggers handleOrderCompleted → ApplyBinArrival
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "lc-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	order = testdb.RequireOrderStatus(t, db, "lc-1", "confirmed")

	// Step 5: Verify bin moved to destination and claim released
	testdb.AssertBinAtNode(t, db, *order.BinID, lineNode.ID)
	testdb.AssertBinUnclaimed(t, db, *order.BinID)
}

// --- TC-2: Staged Complex Order Release ---
// Scenario: verifies staged order release works through the full engine pipeline.
// Creates a complex order with a "wait" step (pickup → dropoff → wait → pickup → dropoff).
// Drives fleet through RUNNING → WAITING so the engine sets DB status to "staged".
// Then sends HandleOrderRelease and verifies post-wait blocks are appended and the
// order completes through the full lifecycle.
func TestSimulator_StagedComplexOrderRelease(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC2")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	env := testEnvelope()
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "staged-tc2",
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

	order := testdb.RequireOrderStatus(t, db, "staged-tc2", dispatch.StatusDispatched)

	// Simulator should have a staged (incomplete) order
	if sim.StagedOrderCount() != 1 {
		t.Fatalf("staged orders = %d, want 1", sim.StagedOrderCount())
	}

	// Pre-wait blocks only (pickup + dropoff = 2 blocks)
	view := sim.GetOrder(order.VendorOrderID)
	if view == nil {
		t.Fatal("simulator should have the staged order")
	}
	if len(view.Blocks) != 2 {
		t.Fatalf("pre-wait blocks = %d, want 2", len(view.Blocks))
	}
	if view.Complete {
		t.Fatal("staged order should not be complete yet")
	}

	// Step 2: Drive RUNNING — robot is moving to first pickup
	sim.DriveState(order.VendorOrderID, "RUNNING")

	order = testdb.RequireOrderStatus(t, db, "staged-tc2", "in_transit")

	// Step 3: Drive WAITING — robot has arrived at wait point and is dwelling.
	// The engine maps WAITING → "staged" and updates the DB.
	sim.DriveState(order.VendorOrderID, "WAITING")

	order = testdb.RequireOrderStatus(t, db, "staged-tc2", dispatch.StatusStaged)

	// Step 4: Edge sends release — appends post-wait blocks
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "staged-tc2",
	})

	// Verify: post-wait blocks were appended (2 pre-wait + 2 post-wait = 4)
	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 4 {
		t.Fatalf("total blocks after release = %d, want 4", len(view.Blocks))
	}
	if !view.Complete {
		t.Fatal("order should be complete after release")
	}

	// All blocks must have bin tasks
	for i, b := range view.Blocks {
		if b.BinTask == "" {
			t.Errorf("block %d (%q) has empty BinTask", i, b.BlockID)
		}
	}

	// Order status should now be in_transit (released from staging)
	order = testdb.RequireOrderStatus(t, db, "staged-tc2", dispatch.StatusInTransit)

	// Step 5: Drive RUNNING → FINISHED to complete the order
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	// Delivered (the call asserts the status; order isn't read after).
	testdb.RequireOrderStatus(t, db, "staged-tc2", "delivered")
}
