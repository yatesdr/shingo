package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
//
// Item 7 (post-Item-8): the DeliveryNode predicate is gone. Removal
// orders flow through the reset like any other completion — runtime
// briefly resets to claim.UOPCapacity. The "different mechanism"
// that catches removals correctly is the reconciler's empty-slot
// detection (Item 4): once the bin physically leaves, Core reports
// no bin at the slot and the reconciler heals runtime to 0. This
// test now exercises the reconciler-heals path; the brief
// looks-like-full window is SME-accepted.
func TestRegression_11_RemovalOrderHealsToZeroViaReconciler(t *testing.T) {
	db := testEngineDB(t)
	nodeID, _, claimID := seedReconcilerNode(t, db, "REG11", "PART-R11")

	// Drain runtime to a partial value — simulates a half-consumed bin.
	const partialUOP = 137
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, partialUOP); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Removal order: process_node is the line (REG11-NODE), but
	// DeliveryNode is OUTBOUND-DEST. The order moves the spent bin
	// AWAY from the line.
	orderID, err := db.CreateOrder("uuid-reg11-removal", orders.TypeComplex,
		&nodeID, false, 1,
		"OUTBOUND-DEST",
		"", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)); err != nil {
		t.Fatalf("set order confirmed: %v", err)
	}
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	// Mock Core reports no bin at the slot — the bin has physically
	// left after the removal completed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{Bins: nil})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	eng.wireEventHandlers()

	// Fire EventOrderCompleted. Without the predicate, this resets
	// the runtime to capacity (200) — the brief looks-like-full UI.
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-reg11-removal",
			OrderType:     orders.TypeComplex,
			ProcessNodeID: &nodeID,
		},
	})

	// Reconciler pass — empty-slot detection (Item 4) zeros the
	// runtime when Core reports no bin at the slot.
	eng.Reconcile(true)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 (post-removal: reconciler observes empty slot and heals runtime to 0)",
			rt.RemainingUOPCached)
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
	if rt.RemainingUOPCached != 200 {
		t.Errorf("RemainingUOP = %d, want 200 (delivery order to process node MUST reset line UOP to claim capacity)",
			rt.RemainingUOPCached)
	}
}

// TestRegression_8_DeliveryAlwaysResetsToCapacityPostItem8 pins the
// post-Item-8 contract: delivery completion ALWAYS resets the runtime
// cache to claim.UOPCapacity, regardless of the bin's actual
// uop_remaining at Core. The reconciler heals the cache to Core's
// authoritative bin value within the next pass (~60s).
//
// Pre-Item-8 the runtime read OrderDelivered.BinUOPRemaining (snapshot
// of the bin's value at delivery time) and reset to that. Item 8
// retired the snapshot — Edge's runtime cache, kept in lockstep with
// Core by the reconciler, is now the source of truth for bin UOP at
// completion time. The trade-off — a brief "looks like full bin" UI
// on partial-back returns until the heal — is SME-accepted.
//
// This test replaces TestRegression_11_DeliveryOfPartialBinResetsToBinUOP
// (deleted with Item 8). The companion reconciler-heals-after-arrival
// behavior is covered by the existing TestRegression_ReconciliationSelfHeal
// in uop_reconciler_test.go.
func TestRegression_8_DeliveryAlwaysResetsToCapacityPostItem8(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "ITEM8", PayloadCode: "PART-I8", UOPCapacity: 200, InitialUOP: 200,
	})

	// Drained runtime — the line was consuming from the previous bin.
	// Pre-arrival, runtime carries 50.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Delivery order to the line. Pre-Item-8 the order would have
	// carried a BinUOPRemaining snapshot; post-Item-8 the field is
	// gone and the reset goes to claim.UOPCapacity unconditionally.
	orderID, err := db.CreateOrder("uuid-item8-delivery", orders.TypeComplex,
		&nodeID, false, 1, "ITEM8-NODE", "", "", "", false, "")
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
			OrderUUID:     "uuid-item8-delivery",
			OrderType:     orders.TypeComplex,
			ProcessNodeID: &nodeID,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 200 {
		t.Errorf("RemainingUOP = %d, want 200 "+
			"(post-Item-8: delivery completion always resets to claim.UOPCapacity; "+
			"reconciler heals to Core's authoritative bin value within the next pass)",
			rt.RemainingUOPCached)
	}
}
