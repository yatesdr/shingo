package engine

import (
	"testing"

	"shingoedge/orders"
)

// TestRegression_11_RemovalOrderDoesNotResetLineUOP exercises the #11
// predicate flip in handleNormalReplenishment.
//
// Pre-fix bug: the function fired for any complex/retrieve order whose
// process_node matched the line, regardless of where the order actually
// delivered. Removal-shaped orders (Order B in two-robot consume, R1
// in press-index, sequential-removal step) take a bin AWAY from the
// line; their completion still spuriously reset RemainingUOP to claim
// capacity, producing phantom inventory turnovers on the HMI while the
// previous bin was still draining.
//
// Post-fix predicate: only fire when ctx.order.DeliveryNode equals
// ctx.node.CoreNodeName. Removal orders have DeliveryNode at storage /
// outbound, so they're correctly skipped.
//
// Setup: a complex order whose process_node IS the line (i.e. the
// runtime tracks consumption against this node) but whose DeliveryNode
// is OUTBOUND (the bin is being evacuated). Drain the runtime to a
// partial value, fire EventOrderCompleted, assert RemainingUOP is
// unchanged. Pre-fix this would fail because the runtime would be
// reset to capacity.
func TestRegression_11_RemovalOrderDoesNotResetLineUOP(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REG11", PayloadCode: "PART-R11", UOPCapacity: 200, InitialUOP: 200,
	})

	// Drain runtime to a partial value — simulates a half-consumed bin.
	const partialUOP = 137
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, partialUOP); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Removal order: process_node is the line (REG11-NODE), but
	// DeliveryNode is OUTBOUND-DEST. This is the shape of Order B
	// in a two-robot consume swap, of R1 in press-index, of the
	// removal step in sequential-removal. The order moves the
	// spent bin AWAY from the line.
	orderID, err := db.CreateOrder("uuid-reg11-removal", orders.TypeComplex,
		&nodeID, false, 1,
		"OUTBOUND-DEST", // DeliveryNode != process_node CoreNodeName
		"", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)); err != nil {
		t.Fatalf("set order confirmed: %v", err)
	}
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-reg11-removal",
			OrderType:     orders.TypeComplex,
			ProcessNodeID: &nodeID,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOP != partialUOP {
		t.Errorf("RemainingUOP = %d, want %d (removal-shaped order must NOT reset line UOP — DeliveryNode != process_node CoreNodeName)",
			rt.RemainingUOP, partialUOP)
	}
}

// TestRegression_11_DeliveryOrderStillResetsLineUOP is the positive
// counterpart: orders that DO deliver to the line still trigger the
// reset. Without this paired test, a future predicate that always
// returns false would silently disable replenishment without any
// regression visible.
func TestRegression_11_DeliveryOrderStillResetsLineUOP(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REG11D", PayloadCode: "PART-R11D", UOPCapacity: 200, InitialUOP: 200,
	})

	// Drained runtime — simulates a half-consumed bin about to be
	// replaced by the incoming delivery.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Delivery order: DeliveryNode IS the process node — Order A in
	// two-robot consume, R2 in press-index, the delivery step in
	// sequential. This DOES turn over the line's UOP tracking.
	orderID, err := db.CreateOrder("uuid-reg11-delivery", orders.TypeComplex,
		&nodeID, false, 1,
		"REG11D-NODE", // DeliveryNode == process_node CoreNodeName
		"", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)); err != nil {
		t.Fatalf("set order confirmed: %v", err)
	}
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-reg11-delivery",
			OrderType:     orders.TypeComplex,
			ProcessNodeID: &nodeID,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOP != 200 {
		t.Errorf("RemainingUOP = %d, want 200 (delivery order to process node MUST reset line UOP to claim capacity)",
			rt.RemainingUOP)
	}
}

// TestRegression_11_DeliveryOfPartialBinResetsToBinUOP locks the
// partial-return loop on the line side. Bin_A coming IN from supermarket
// can be a partial — its uop_remaining was set by a prior cycle's
// RELEASE PARTIAL and preserved through transit. When the bin arrives
// at the line, runtime must reset to the bin's authoritative
// uop_remaining (captured in OrderDelivered.BinUOPRemaining at delivery
// time and persisted on the order row), NOT to claim.UOPCapacity.
//
// Without this, a partial bin returning to the line via a swap would
// land showing fresh-full inventory on the operator HMI while
// physically holding only the drained value — the same shape as
// pre-#11 phantom turnovers, but driven from the bin record side
// instead of the predicate side.
//
// Pre-#11: regardless of BinUOPRemaining, runtime reset to capacity.
// Post-#11: runtime reads BinUOPRemaining if present, falls back to
// claim.UOPCapacity otherwise. This test pins the BinUOPRemaining
// branch so a future refactor that drops it surfaces here, not at a
// plant.
//
// Companion to TestWiring_RetrieveCompletion_ConsumePartialBin_ResetsToBinUOP
// in wiring_test.go which covers the simple-retrieve form. This one
// covers the complex-order form (Order A in a two-robot consume swap)
// which is the production-relevant shape.
func TestRegression_11_DeliveryOfPartialBinResetsToBinUOP(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REG11P", PayloadCode: "PART-R11P", UOPCapacity: 200, InitialUOP: 200,
	})

	// Drained runtime — the line had been consuming from the previous
	// bin. Pre-arrival of bin_A this is the value the runtime carries.
	// Seed to a value that's distinct from both capacity (200) and the
	// expected post-arrival value (125) so a clobber to either is
	// visible.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Delivery order: bin_A delivers to the line, but bin_A is a
	// PARTIAL bin from a prior cycle's RELEASE PARTIAL — its
	// uop_remaining is 125, not capacity. Core captures that value in
	// OrderDelivered.BinUOPRemaining at FINISHED time; Edge persists
	// it on the order row via UpdateOrderBinUOPRemaining (production
	// path: HandleDeliveredWithExpiry).
	orderID, err := db.CreateOrder("uuid-reg11-partial-in", orders.TypeComplex,
		&nodeID, false, 1,
		"REG11P-NODE", // DeliveryNode == process_node CoreNodeName
		"", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)); err != nil {
		t.Fatalf("set order confirmed: %v", err)
	}
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	// Snapshot bin_A's authoritative uop_remaining at delivery time.
	// 125 is < capacity (200) and != the seeded runtime (50), so the
	// assertion can't be falsely satisfied by a clobber to either.
	const partialBinUOP = 125
	v := partialBinUOP
	if err := db.UpdateOrderBinUOPRemaining(orderID, &v); err != nil {
		t.Fatalf("set bin_uop_remaining: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-reg11-partial-in",
			OrderType:     orders.TypeComplex,
			ProcessNodeID: &nodeID,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOP != partialBinUOP {
		t.Errorf("RemainingUOP = %d, want %d "+
			"(partial-in delivery: runtime must reset to bin's authoritative uop_remaining "+
			"from OrderDelivered.BinUOPRemaining snapshot, NOT to claim.UOPCapacity 200; "+
			"this is the line side of the partial-return loop)",
			rt.RemainingUOP, partialBinUOP)
	}
}
