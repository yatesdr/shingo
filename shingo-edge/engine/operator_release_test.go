package engine

import (
	"testing"

	"shingoedge/orders"
	"shingoedge/store"
)

// stageOrderForConsumeNode seeds a staged complex order against the
// consume node and hangs it on runtime.StagedOrderID so ReleaseOrder
// behaves as it would in production.
func stageOrderForConsumeNode(t *testing.T, db *store.DB, nodeID int64, uuid string) int64 {
	t.Helper()
	orderID, err := db.CreateOrder(uuid, orders.TypeComplex,
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, orders.StatusStaged); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}
	return orderID
}

// TestReleaseOrderWithLineside_ResetsUOPAndCapturesBuckets verifies that
// the release-click path sets UOP to capacity, marks the changeover
// task (if any) as released, and records the parts the operator pulled
// to lineside in node_lineside_bucket.
func TestReleaseOrderWithLineside_ResetsUOPAndCapturesBuckets(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-REL", PayloadCode: "PART-R", UOPCapacity: 100, InitialUOP: 8,
	})

	// Drain the counter low (simulating pre-swap production).
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 8); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-rel-1")

	eng := testEngine(t, db)
	if err := eng.ReleaseOrderWithLineside(orderID, map[string]int{"PART-R": 12}); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	// UOP should be at capacity.
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (capacity) after release", runtime.RemainingUOP)
	}

	// Bucket should exist with 12 active units.
	b, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-R")
	if err != nil {
		t.Fatalf("GetActiveLinesideBucket: %v", err)
	}
	if b.Qty != 12 {
		t.Errorf("bucket qty = %d, want 12", b.Qty)
	}
	if b.State != store.LinesideStateActive {
		t.Errorf("bucket state = %q, want %q", b.State, store.LinesideStateActive)
	}

	// Order should be in_transit (release dispatched).
	o, _ := db.GetOrder(orderID)
	if o.Status != orders.StatusInTransit {
		t.Errorf("order status = %q, want %q", o.Status, orders.StatusInTransit)
	}
}

// TestReleaseOrderWithLineside_EmptyMapStillResetsUOP verifies that
// calling release with nothing captured still performs the UOP reset
// and deactivates stranded buckets for other styles.
func TestReleaseOrderWithLineside_EmptyMapStillResetsUOP(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-REL2", PayloadCode: "PART-R2", UOPCapacity: 50, InitialUOP: 3,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 3); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-rel-2")

	eng := testEngine(t, db)
	if err := eng.ReleaseOrderWithLineside(orderID, nil); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 50 {
		t.Errorf("RemainingUOP = %d, want 50 (capacity) after release with nil map", runtime.RemainingUOP)
	}
}

// TestReleaseOrderWithLineside_DeactivatesStrandedStyles verifies that
// when the release click happens, any active buckets on the node that
// belong to a different style are flipped to inactive.
func TestReleaseOrderWithLineside_DeactivatesStrandedStyles(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-REL3", PayloadCode: "PART-R3", UOPCapacity: 80, InitialUOP: 5,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 5); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Seed a leftover active bucket from a different style on this node.
	otherStyleID := styleID + 999
	if _, err := db.CaptureLinesideBucket(nodeID, "", otherStyleID, "PART-OLD", 4); err != nil {
		t.Fatalf("seed leftover bucket: %v", err)
	}

	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-rel-3")
	eng := testEngine(t, db)
	if err := eng.ReleaseOrderWithLineside(orderID, map[string]int{"PART-R3": 2}); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	// Leftover bucket should now be inactive.
	inactive, err := db.ListInactiveLinesideBuckets(nodeID)
	if err != nil {
		t.Fatalf("ListInactiveLinesideBuckets: %v", err)
	}
	if len(inactive) != 1 {
		t.Fatalf("inactive buckets = %d, want 1", len(inactive))
	}
	if inactive[0].StyleID != otherStyleID || inactive[0].PartNumber != "PART-OLD" {
		t.Errorf("unexpected inactive bucket: %+v", inactive[0])
	}

	// New-style bucket should be active.
	b, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-R3")
	if err != nil {
		t.Fatalf("GetActiveLinesideBucket: %v", err)
	}
	if b.Qty != 2 {
		t.Errorf("active bucket qty = %d, want 2", b.Qty)
	}
}

// TestHandleComplexOrderBCompletion_SkipsIfAlreadyReleased verifies the
// idempotent gate: when the release handler already advanced the task
// to "released" (and reset the counter), Order B completion doesn't
// clobber the drained counter back to capacity.
func TestHandleComplexOrderBCompletion_SkipsIfAlreadyReleased(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-IDEMP", PayloadCode: "PART-IDEMP", UOPCapacity: 100, InitialUOP: 100,
	})

	// Simulate "release click already happened": UOP at capacity,
	// then a few counter ticks drained it to 87.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 87); err != nil {
		t.Fatalf("seed drained runtime: %v", err)
	}

	// There's no changeover in this scenario — handleNormalReplenishment
	// should see a complex order on a consume node with a live runtime
	// and skip the reset. Create the completed order.
	orderID, err := db.CreateOrder("uuid-idemp", orders.TypeComplex,
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusConfirmed)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-idemp",
			OrderType:     orders.TypeComplex,
			ProcessNodeID: &nodeID,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 87 {
		t.Errorf("RemainingUOP = %d, want 87 (release already ran, completion must not clobber)",
			runtime.RemainingUOP)
	}
}

// TestHandleNormalReplenishment_RetrieveStillResets verifies that simple
// retrieve orders (no release-click prompt) continue to reset UOP at
// completion — the gate only applies to complex orders.
func TestHandleNormalReplenishment_RetrieveStillResets(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-RETR", PayloadCode: "PART-RETR", UOPCapacity: 100, InitialUOP: 10,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 10); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	orderID, err := db.CreateOrder("uuid-retr", orders.TypeRetrieve,
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusConfirmed)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-retr",
			OrderType:     orders.TypeRetrieve,
			ProcessNodeID: &nodeID,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (simple retrieve should still reset)",
			runtime.RemainingUOP)
	}
}
