package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
)

// TestBinOwnership_BinAtNodeReadsRuntimeActiveBinID pins the post-flip
// contract: binAtNode resolves the bin pointer from
// runtime.ActiveBinID, NOT by walking through the active order's BinID.
// The test seeds an order with a different BinID than the runtime's
// ActiveBinID and asserts attribution lands on the runtime's pointer.
// This is the load-bearing invariant for the bin-ownership flip — the
// order pointer can come and go (pickup, completion, manual clear),
// but the bin pointer survives until the bin physically leaves.
func TestBinOwnership_BinAtNodeReadsRuntimeActiveBinID(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "OWN-BAN",
		PayloadCode: "PART-OWN",
		UOPCapacity: 100,
		InitialUOP:  100,
	})

	// Set runtime's bin pointer to one value.
	const runtimeBin int64 = 7777
	bid := runtimeBin
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 100); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Create an order with a DIFFERENT BinID — proves binAtNode does not
	// walk the order. Pre-flip, this would have returned the order's
	// BinID; post-flip it returns runtime.ActiveBinID.
	const orderBin int64 = 9999
	orderID, err := db.CreateOrder("uuid-own-ban", orders.TypeRetrieve,
		&nodeID, false, 1, "OWN-BAN-NODE", "", "", "", false, "PART-OWN")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	stale := orderBin
	_ = db.UpdateOrderBinID(orderID, &stale)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	eng.Events.Emit(Event{
		Type: EventCounterDelta,
		Payload: CounterDeltaEvent{
			ProcessID: processID,
			StyleID:   styleID,
			Delta:     5,
		},
	})

	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1", len(sink.binCalls))
	}
	if got := sink.binCalls[0].BinID; got != runtimeBin {
		t.Errorf("bin call BinID = %d, want %d (runtime.ActiveBinID, NOT order.BinID=%d)",
			got, runtimeBin, orderBin)
	}
}

// TestBinOwnership_PickupClearsActiveBinID pins the symmetric clear:
// when a bin is picked up, the runtime's ActiveBinID must be nil so
// PLC ticks during the gap before the next delivery don't silently
// attribute to the departed bin.
func TestBinOwnership_PickupClearsActiveBinID(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "OWN-PICK",
		PayloadCode: "PART-PICK",
		UOPCapacity: 100,
		InitialUOP:  50,
	})

	const binID int64 = 8888
	const orderUUID = "uuid-own-pick"
	bid := binID
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "OWN-PICK-NODE", "", "", "", false, "PART-PICK")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 50); err != nil {
		t.Fatalf("seed runtime with bin: %v", err)
	}

	// Pre-condition: bin pointer set.
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Fatalf("pre-pickup ActiveBinID = %v, want %d", rt.ActiveBinID, binID)
	}

	eng := testEngine(t, db)
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	eng.HandleBinPickedUp(orderUUID, binID)

	// Post-condition: bin pointer cleared symmetric with order pointer.
	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID != nil {
		t.Errorf("post-pickup ActiveBinID = %v, want nil (bin physically left the slot)",
			rt.ActiveBinID)
	}
}

// TestBinOwnership_DeliveryThenTickThenPickup is the round-trip
// regression: a complete bin lifecycle through Edge — delivery sets
// active_bin_id, PLC tick ships a delta against that bin, pickup
// clears the pointer, post-pickup tick ships nothing.
func TestBinOwnership_DeliveryThenTickThenPickup(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "OWN-RT",
		PayloadCode: "PART-RT",
		UOPCapacity: 100,
		InitialUOP:  0, // Pre-delivery: empty slot.
	})
	// Clear the default seed bin so this test starts from a true empty slot.
	_ = db.SetProcessNodeActiveBinID(nodeID, nil)

	const binID int64 = 5555
	const orderUUID = "uuid-own-rt"
	bid := binID
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "OWN-RT-NODE", "", "", "", false, "PART-RT")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed))
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	// Phase 1: delivery handler fires (emitOrderCompleted helper now
	// emits EventOrderDelivered first under the new contract).
	// handleNodeOrderDelivered → SetProcessNodeRuntimeForDeliveredBin
	// sets active_bin_id, cached_bin_id, and remaining_uop_cached.
	emitOrderCompleted(eng, orderID, orderUUID, orders.TypeRetrieve, &nodeID)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Fatalf("after delivery: ActiveBinID = %v, want %d", rt.ActiveBinID, binID)
	}

	// Phase 2: PLC tick emits a bin delta against the delivered bin.
	// resolveStyleID via active claim → fire EventCounterDelta.
	claim, _ := db.GetStyleNodeClaim(*rt.ActiveClaimID)
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: claim.StyleID, Delta: 3,
	}})

	if len(sink.binCalls) != 1 {
		t.Fatalf("post-delivery bin calls = %d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	if got := sink.binCalls[0]; got.BinID != binID || got.Delta != -3 || got.Reason != protocol.ReasonConsumeTick {
		t.Errorf("post-delivery bin call mismatch: %+v (want bin=%d delta=-3 reason=consume_tick)",
			got, binID)
	}

	// Phase 3: pickup. ActiveBinID clears.
	eng.HandleBinPickedUp(orderUUID, binID)

	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID != nil {
		t.Errorf("post-pickup ActiveBinID = %v, want nil", rt.ActiveBinID)
	}

	// Phase 4: post-pickup tick — must NOT emit a bin delta. The slot
	// is empty; attributing to the prior bin would be a false count.
	preCount := len(sink.binCalls)
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: claim.StyleID, Delta: 7,
	}})
	if len(sink.binCalls) != preCount {
		t.Errorf("post-pickup bin calls = %d, want %d (no bin → no delta): new=%+v",
			len(sink.binCalls), preCount, sink.binCalls[preCount:])
	}
}
