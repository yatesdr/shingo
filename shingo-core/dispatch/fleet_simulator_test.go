//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
)

// =============================================================================
// Fleet Simulator Regression Tests
//
// These tests use the in-memory SimulatorBackend to verify that the dispatcher
// sends the correct blocks and bin tasks to the fleet backend — without
// requiring real robots or an RDS server.
//
// Each test targets a specific bug or failure mode observed in production.
// =============================================================================

// TC-1: Complex order blocks must include JackLoad/JackUnload bin tasks.
// Scenario: verifies every fleet block includes a bin task (JackLoad/JackUnload).
//
// Bug: 2026-03-26 — stepsToBlocks() was creating OrderBlocks without BinTask,
// causing robots to navigate to locations without actually jacking bins.
// This test would have caught it in CI.
func TestSimulator_ComplexOrderBinTasks(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at storage with a confirmed manifest
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC1")

	sim := simulator.New()
	d, _ := newTestDispatcher(t, db, sim)

	env := testEnvelope()
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-tc1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	order, err := db.GetOrderByUUID("complex-tc1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.VendorOrderID == "" {
		t.Fatal("order should have a vendor order ID")
	}

	// Inspect: every block must have a bin task
	blocks := sim.BlocksForOrder(order.VendorOrderID)
	if len(blocks) < 2 {
		t.Fatalf("expected >= 2 blocks, got %d", len(blocks))
	}

	for _, b := range blocks {
		if b.BinTask == "" {
			t.Errorf("block %q at %q has empty BinTask — robots would navigate without jacking", b.BlockID, b.Location)
		}
		if b.BinTask != "JackLoad" && b.BinTask != "JackUnload" {
			t.Errorf("block %q has unexpected BinTask %q", b.BlockID, b.BinTask)
		}
	}

	// Verify pickup block is at storage and dropoff at line
	foundPickup := false
	foundDropoff := false
	for _, b := range blocks {
		if b.Location == storageNode.Name && b.BinTask == "JackLoad" {
			foundPickup = true
		}
		if b.Location == lineNode.Name && b.BinTask == "JackUnload" {
			foundDropoff = true
		}
	}
	if !foundPickup {
		t.Error("no JackLoad block at storage node found")
	}
	if !foundDropoff {
		t.Error("no JackUnload block at line node found")
	}
}

// TC-2: Staged complex order — pre-wait blocks sent initially,
// post-wait blocks appended on release.
// Scenario: verifies dispatcher-level staged order block structure.
//
// Complex orders with a "wait" step are dispatched as staged (incomplete)
// orders. When the robot reaches the wait point, Edge sends a release
// and the remaining blocks are appended.
func TestSimulator_StagedComplexOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC2")

	sim := simulator.New()
	d, _ := newTestDispatcher(t, db, sim)

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

	order, err := db.GetOrderByUUID("staged-tc2")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Order should be dispatched
	if order.Status != StatusDispatched {
		t.Fatalf("order status = %q, want %q", order.Status, StatusDispatched)
	}

	// Verify: simulator has 1 staged (incomplete) order
	if sim.StagedOrderCount() != 1 {
		t.Errorf("staged orders = %d, want 1", sim.StagedOrderCount())
	}

	// Verify: initial blocks are only pre-wait steps (2 blocks)
	view := sim.GetOrder(order.VendorOrderID)
	if view == nil {
		t.Fatal("simulator should have the staged order")
	}
	if len(view.Blocks) != 2 {
		t.Fatalf("pre-wait blocks = %d, want 2 (pickup + dropoff)", len(view.Blocks))
	}
	if view.Complete {
		t.Error("staged order should not be complete yet")
	}

	// All pre-wait blocks must have bin tasks
	for i, b := range view.Blocks {
		if b.BinTask == "" {
			t.Errorf("pre-wait block %d (%q) has empty BinTask", i, b.BlockID)
		}
	}

	// In a full engine test the fleet tracker would transition the order to
	// "staged" when the robot reports WAITING.  This dispatcher-level test has
	// no engine wiring, so we advance the status manually.
	if err := db.UpdateOrderStatus(order.ID, string(StatusStaged), "robot waiting (simulated)"); err != nil {
		t.Fatalf("set order staged: %v", err)
	}

	// Release: append post-wait blocks
	d.HandleOrderRelease(env, &protocol.OrderRelease{
		OrderUUID: "staged-tc2",
	})

	// Verify: post-wait blocks were appended
	view = sim.GetOrder(order.VendorOrderID)
	if len(view.Blocks) != 4 {
		t.Fatalf("total blocks after release = %d, want 4", len(view.Blocks))
	}
	if !view.Complete {
		t.Error("order should be complete after release")
	}

	// All blocks must have bin tasks
	for i, b := range view.Blocks {
		if b.BinTask == "" {
			t.Errorf("block %d (%q) has empty BinTask after release", i, b.BlockID)
		}
	}
}

// TC-3: Simple retrieve.
// Scenario: verifies the full dispatch path creates the right fleet request
// with JackLoad at source and JackUnload at destination.
func TestSimulator_SimpleRetrieveOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC3")

	sim := simulator.New()
	d, emitter := newTestDispatcher(t, db, sim)

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-tc3",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	// Verify: order was dispatched
	order, err := db.GetOrderByUUID("retrieve-tc3")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Fatalf("status = %q, want %q", order.Status, StatusDispatched)
	}
	if len(emitter.dispatched) != 1 {
		t.Fatalf("dispatched events = %d, want 1", len(emitter.dispatched))
	}

	// Verify: simulator received a transport order with 2 blocks
	view := sim.GetOrderByIndex(0)
	if view == nil {
		t.Fatal("simulator should have received the order")
	}
	if len(view.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(view.Blocks))
	}

	// Verify: correct bin tasks and locations
	load := view.Blocks[0]
	unload := view.Blocks[1]
	if load.BinTask != "JackLoad" {
		t.Errorf("block 0 BinTask = %q, want JackLoad", load.BinTask)
	}
	if load.Location != storageNode.Name {
		t.Errorf("block 0 Location = %q, want %q", load.Location, storageNode.Name)
	}
	if unload.BinTask != "JackUnload" {
		t.Errorf("block 1 BinTask = %q, want JackUnload", unload.BinTask)
	}
	if unload.Location != lineNode.Name {
		t.Errorf("block 1 Location = %q, want %q", unload.Location, lineNode.Name)
	}
}

// TC-4: Simulator state mapping matches RDS adapter mapping.
// Scenario: verifies simulator state mapping is identical to the real RDS adapter.
//
// Ensures the simulator's MapState matches the real SEER RDS adapter
// exactly, so state transitions emitted in tests are realistic.
func TestSimulator_StateMapping(t *testing.T) {
	t.Parallel()
	sim := simulator.New()

	tests := []struct {
		vendor    string
		wantState string
		terminal  bool
	}{
		{"CREATED", "dispatched", false},
		{"TOBEDISPATCHED", "dispatched", false},
		{"RUNNING", "in_transit", false},
		{"WAITING", "staged", false},
		{"FINISHED", "delivered", true},
		{"FAILED", "failed", true},
		{"STOPPED", "cancelled", true},
	}

	for _, tt := range tests {
		mapped := sim.MapState(tt.vendor)
		if mapped != tt.wantState {
			t.Errorf("MapState(%q) = %q, want %q", tt.vendor, mapped, tt.wantState)
		}
		terminal := sim.IsTerminalState(tt.vendor)
		if terminal != tt.terminal {
			t.Errorf("IsTerminalState(%q) = %v, want %v", tt.vendor, terminal, tt.terminal)
		}
	}
}

// TC-5: Fleet creation failure causes order to fail with no vendor order ID.
// Scenario: verifies that fleet rejection results in a clean failed order
// with no phantom return orders.
//
// Bug: maybeCreateReturnOrder was creating spurious return orders for orders
// that failed before the fleet accepted them (empty VendorOrderID).
func TestSimulator_FleetFailure_NoVendorOrderID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-TC5")

	sim := simulator.New(simulator.WithCreateFailure())
	d, _ := newTestDispatcher(t, db, sim)

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-fail-tc5",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1,
	})

	order, err := db.GetOrderByUUID("retrieve-fail-tc5")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

	// Order should have failed because the fleet rejected it
	if order.Status != StatusFailed {
		t.Errorf("order status = %q, want %q", order.Status, StatusFailed)
	}

	// The vendor order ID must be empty — fleet never accepted
	if order.VendorOrderID != "" {
		t.Errorf("vendor order ID = %q, want empty (fleet never accepted)", order.VendorOrderID)
	}

	// No orders should exist in the simulator
	if sim.OrderCount() != 0 {
		t.Errorf("simulator has %d orders, want 0 (fleet rejected creation)", sim.OrderCount())
	}
}
