//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// --- Robot ID persistence tests for handleVendorStatusChange (Option C) ---
//
// These tests exercise the robot ID path through handleVendorStatusChange
// using DriveStateWithRobot, which simulates the real fleet backend's behavior
// of including the vehicle identifier in every status event.
//
// Without DriveStateWithRobot, Case D (clobbering existing robot_id) was
// untestable because DriveState always passed robotID="".
//
// This suite covers: first assignment, Case D regression, reassignment,
// idempotent no-write, Option C dedup, and narrow write verification.

// dispatchRetrieveOrderWithRobot is a variant of dispatchRetrieveOrder that
// returns only the values needed for robot ID tests.
func dispatchRetrieveOrderWithRobot(t *testing.T) (*store.DB, *Engine, *simulator.SimulatorBackend, *orders.Order) {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RID-1")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "rid-order-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name,
		Quantity:     1,
	})

	order := testdb.RequireOrderStatus(t, db, "rid-order-1", dispatch.StatusDispatched)
	return db, eng, sim, order
}

// First robot assignment persists robot ID and sends waybill.
// This is Case A: new robot ID + status change (dispatched -> in_transit).
// Option C: waybill sent, then single UpdateOrderVendor with effectiveRobotID.
func TestVendorStatus_RobotID_FirstAssignment(t *testing.T) {
	t.Parallel()
	db, _, sim, order := dispatchRetrieveOrderWithRobot(t)

	// Drive RUNNING with a real robot ID (matches production RDS behavior)
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-42")

	got := testdb.AssertOrderStatus(t, db, "rid-order-1", "in_transit")
	if got.RobotID != "AMB-42" {
		t.Errorf("robot_id: got %q, want %q", got.RobotID, "AMB-42")
	}

	// Verify a waybill carrying the assigned robot was sent. NOTE: post the
	// claim-move to the scanner a simple order dispatches through the fulfillment
	// scanner, which emits a route waybill at dispatch (robot_id empty — no robot
	// yet), matching the scanner's long-standing behavior for queued orders. The
	// robot-assignment waybill (this test's subject) is a SECOND waybill, so scan
	// for the one that
	// carries AMB-42 rather than asserting on the first waybill found.
	outbox, _ := db.ListPendingOutbox(50)
	var foundWaybillWithRobot bool
	for _, msg := range outbox {
		if msg.MsgType != protocol.TypeOrderWaybill || msg.StationID != order.StationID {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(msg.Payload, &env); err != nil {
			continue
		}
		var wb protocol.OrderWaybill
		if err := json.Unmarshal(env.Payload, &wb); err != nil {
			continue
		}
		if wb.RobotID == "AMB-42" {
			foundWaybillWithRobot = true
			break
		}
	}
	if !foundWaybillWithRobot {
		t.Error("expected a waybill carrying robot_id AMB-42 after first robot assignment")
	}
}

// Case D regression - subsequent event with empty RobotID does NOT clobber.
// Robot is assigned on RUNNING, then FINISHED event arrives with empty RobotID.
// Option C effectiveRobotID preserves the existing value.
func TestVendorStatus_RobotID_CaseD_NoClobber(t *testing.T) {
	t.Parallel()
	db, _, sim, order := dispatchRetrieveOrderWithRobot(t)

	// Step 1: Assign robot on RUNNING
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-42")

	got := testdb.RequireOrder(t, db, "rid-order-1")
	if got.RobotID != "AMB-42" {
		t.Fatalf("robot_id after RUNNING: got %q, want %q", got.RobotID, "AMB-42")
	}

	// Step 2: Drive FINISHED with empty robot ID (simulates DriveState, not DriveStateWithRobot)
	// This is the exact scenario that caused Case D before the fix.
	sim.DriveState(order.VendorOrderID, "FINISHED")

	got = testdb.AssertOrderStatus(t, db, "rid-order-1", "delivered")
	// The critical assertion: robot_id must be preserved, not clobbered to empty
	if got.RobotID != "AMB-42" {
		t.Errorf("Case D regression: robot_id was clobbered to %q, want %q (preserved)", got.RobotID, "AMB-42")
	}
}

// Robot reassignment - event with different non-empty RobotID updates.
// This is Case D reassignment variant: order already has AMB-42, event carries AMB-99.
func TestVendorStatus_RobotID_Reassignment(t *testing.T) {
	t.Parallel()
	db, _, sim, order := dispatchRetrieveOrderWithRobot(t)

	// Step 1: Assign robot on RUNNING
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-42")

	got := testdb.RequireOrder(t, db, "rid-order-1")
	if got.RobotID != "AMB-42" {
		t.Fatalf("robot_id after RUNNING: got %q, want %q", got.RobotID, "AMB-42")
	}

	// Step 2: Robot is reassigned - fleet sends WAITING with new robot
	sim.DriveStateWithRobot(order.VendorOrderID, "WAITING", "AMB-99")

	got = testdb.AssertOrderStatus(t, db, "rid-order-1", "staged")
	if got.RobotID != "AMB-99" {
		t.Errorf("robot_id after reassignment: got %q, want %q", got.RobotID, "AMB-99")
	}
}

// Idempotent no-write - same status + same robot = no state change.
// Driving RUNNING twice with same robot ID should be a no-op on the second call.
func TestVendorStatus_RobotID_IdempotentNoChange(t *testing.T) {
	t.Parallel()
	db, _, sim, order := dispatchRetrieveOrderWithRobot(t)

	// Step 1: First RUNNING with robot
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-42")

	got1 := testdb.RequireOrder(t, db, "rid-order-1")
	if got1.RobotID != "AMB-42" {
		t.Fatalf("robot_id after first RUNNING: got %q, want %q", got1.RobotID, "AMB-42")
	}
	updatedAt1 := got1.UpdatedAt

	// Step 2: Drive RUNNING again with same robot (idempotent)
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-42")

	got2 := testdb.AssertOrderStatus(t, db, "rid-order-1", "in_transit")
	if got2.RobotID != "AMB-42" {
		t.Errorf("robot_id after double RUNNING: got %q, want %q", got2.RobotID, "AMB-42")
	}
	// The idempotent path (same status + same robot) should not write to DB,
	// so updated_at should not change. Note: this relies on Postgres NOW()
	// being the same within the same second. If this flakes, it means the
	// idempotent path IS writing (a bug).
	if !got2.UpdatedAt.Equal(updatedAt1) {
		t.Errorf("updated_at changed on idempotent event - idempotent path may be writing unnecessarily")
	}
}

// Option C dedup - first robot assignment + status change = single UpdateOrderVendor.
// On the dispatched -> in_transit path with a new robot, Option C should produce
// exactly one UpdateOrderVendor call (not two like the old code). We verify by
// checking that vendor_state matches the RUNNING event state.
func TestVendorStatus_RobotID_OptionC_SingleWrite(t *testing.T) {
	t.Parallel()
	db, _, sim, order := dispatchRetrieveOrderWithRobot(t)

	// Drive RUNNING with robot - Option C path: waybill + single UpdateOrderVendor
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-42")

	got := testdb.AssertOrderStatus(t, db, "rid-order-1", "in_transit")
	if got.RobotID != "AMB-42" {
		t.Errorf("robot_id: got %q, want %q", got.RobotID, "AMB-42")
	}
	// vendor_state should be RUNNING (the event's NewStatus)
	if got.VendorState != "RUNNING" {
		t.Errorf("vendor_state: got %q, want %q (single write, not clobbered)", got.VendorState, "RUNNING")
	}
	// vendor_order_id should still be set
	if got.VendorOrderID != order.VendorOrderID {
		t.Errorf("vendor_order_id: got %q, want %q", got.VendorOrderID, order.VendorOrderID)
	}
}

// Idempotent path uses narrow UpdateOrderRobotID when robot changes without status change.
// If the status is the same but the robot ID differs, the narrow UpdateOrderRobotID
// method should be used (only touches robot_id, not vendor_state or vendor_order_id).
func TestVendorStatus_RobotID_NarrowWrite_SameStatusNewRobot(t *testing.T) {
	t.Parallel()
	db, eng, sim, order := dispatchRetrieveOrderWithRobot(t)

	// Step 1: RUNNING with robot AMB-42
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-42")

	got1 := testdb.RequireOrder(t, db, "rid-order-1")
	if got1.VendorState != "RUNNING" {
		t.Fatalf("vendor_state after RUNNING: got %q, want %q", got1.VendorState, "RUNNING")
	}

	// Step 2: Call handleVendorStatusChange directly with same status but different robot.
	// The simulator's DriveState silently drops duplicate state transitions
	// (oldState == newState), so we can't exercise the idempotent robot-change
	// path through the event pipeline. In production, the fleet backend CAN
	// send the same state twice with a different robot ID (robot reassignment
	// mid-mission). We call the handler directly to test this path.
	eng.handleVendorStatusChange(OrderStatusChangedEvent{
		OrderID:       order.ID,
		VendorOrderID: order.VendorOrderID,
		OldStatus:     "RUNNING",
		NewStatus:     "RUNNING",
		RobotID:       "AMB-99",
	})

	got2 := testdb.RequireOrder(t, db, "rid-order-1")
	if got2.RobotID != "AMB-99" {
		t.Errorf("robot_id after reassignment on idempotent path: got %q, want %q", got2.RobotID, "AMB-99")
	}
	// vendor_state should NOT have been rewritten (narrow write only touches robot_id)
	if got2.VendorState != got1.VendorState {
		t.Errorf("vendor_state changed on narrow write: was %q, now %q (should be untouched)", got1.VendorState, got2.VendorState)
	}
	if got2.VendorOrderID != got1.VendorOrderID {
		t.Errorf("vendor_order_id changed on narrow write: was %q, now %q (should be untouched)", got1.VendorOrderID, got2.VendorOrderID)
	}
}

// TestSimpleDispatch_EmitsAckAndWaybill is the gate (red-before-green) for the
// fleet-create collapse. Routing the simple no-wait dispatch through the unified
// CreateOrder primitive inside dispatchToFleetCore (the scanner's DispatchDirect
// path) MUST preserve the scanner's dispatch-time Edge notifications —
// notifyEdgeDispatched emits a TypeOrderAck + TypeOrderWaybill on dispatch.
// Routing simple through the complex tail (DispatchPreparedComplex) instead would
// emit only EmitOrderDispatched (tracker-only, wiring.go:65-78) and silently DROP
// the dispatch-time ack+waybill.
//
// The fleet mock is blind to the outbox (ack+waybill are outbox messages, not
// fleet calls), so this asserts at the outbox level — the one place a regression
// is visible. A green here means the collapse preserved the Edge contract.
func TestSimpleDispatch_EmitsAckAndWaybill(t *testing.T) {
	t.Parallel()
	db, _, _, order := dispatchRetrieveOrderWithRobot(t)

	// The order dispatched through the scanner (CreateOrder, Complete=true); the
	// dispatch-time ack + waybill must be in the outbox addressed to its station.
	outbox, _ := db.ListPendingOutbox(50)
	var (
		gotAck     bool
		gotWaybill bool
	)
	for _, msg := range outbox {
		if msg.MsgType != protocol.TypeOrderAck && msg.MsgType != protocol.TypeOrderWaybill {
			continue
		}
		if msg.StationID != order.StationID {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(msg.Payload, &env); err != nil {
			continue
		}
		switch msg.MsgType {
		case protocol.TypeOrderAck:
			var ack protocol.OrderAck
			if json.Unmarshal(env.Payload, &ack) == nil && ack.OrderUUID == order.EdgeUUID {
				gotAck = true
			}
		case protocol.TypeOrderWaybill:
			var wb protocol.OrderWaybill
			if json.Unmarshal(env.Payload, &wb) == nil && wb.OrderUUID == order.EdgeUUID {
				gotWaybill = true
			}
		}
	}
	if !gotAck {
		t.Error("expected a dispatch-time TypeOrderAck in the outbox for the simple order — " +
			"the collapse must preserve notifyEdgeDispatched (routing through DispatchPreparedComplex drops it)")
	}
	if !gotWaybill {
		t.Error("expected a dispatch-time TypeOrderWaybill in the outbox for the simple order — " +
			"the collapse must preserve notifyEdgeDispatched (routing through DispatchPreparedComplex drops it)")
	}
}
