package engine

import (
	"testing"

	"shingoedge/orders"
)

// TestBinPickedUp_FlushesAccumulator pins Item 11's flush trigger:
// when the robot picks up the released bin, the inventory delta
// accumulator must flush so any in-flight ticks ship before the
// active claim advances. Without the flush, Edge can lose a tick
// or two recorded between RELEASE PARTIAL and the physical pickup.
func TestBinPickedUp_FlushesAccumulator(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "BPU-FLUSH",
		PayloadCode: "PART-BPU",
		UOPCapacity: 100,
		InitialUOP:  50,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	const binID int64 = 11001
	const orderUUID = "uuid-bpu-flush"
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "BPU-FLUSH-NODE", "", "", "", false, "PART-BPU")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	// Pre-condition: zero flushes recorded.
	if sink.flushes != 0 {
		t.Fatalf("pre-handler flushes = %d, want 0", sink.flushes)
	}

	eng.HandleBinPickedUp(orderUUID, binID)

	if sink.flushes == 0 {
		t.Errorf("Flush() not called — BinPickedUp must flush the released bin's accumulator")
	}

	// Post-condition: ActiveOrderID cleared so subsequent ticks
	// attribute cleanly to whatever lands next.
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveOrderID != nil {
		t.Errorf("ActiveOrderID = %v, want nil (handler must clear active so next tick attribution is clean)",
			rt.ActiveOrderID)
	}
}

// TestBinPickedUp_HandlesMissingOrder pins the missing-order path:
// when the OrderUUID doesn't match a local order (terminal-order GC,
// cross-station envelope, etc.) the handler logs and returns
// without crashing.
func TestBinPickedUp_HandlesMissingOrder(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	// No order seeded — handler must early-return cleanly.
	eng.HandleBinPickedUp("uuid-nonexistent", 99999)

	// No flush should fire for an unknown order — there's nothing to
	// flush against and the runtime is untouched. The tested path is
	// graceful degradation, not a hard error.
	if sink.flushes != 0 {
		t.Errorf("Flush() called for unknown order: flushes = %d, want 0", sink.flushes)
	}
}

// TestRegression_PartialBackTicksAttributeToReleasedBin pins the
// motivating Item 11 invariant end-to-end at the engine layer: a
// RELEASE PARTIAL leaves the bin at the line, ticks fire, and they
// all attribute to the released bin's BinID until BinPickedUp clears
// the active order. After BinPickedUp, ActiveOrderID is nil so the
// next tick from handleConsumeTick can't find a bin to attribute to
// (binAtNode returns 0 → no bin delta), preventing mis-attribution
// to a bin that's no longer at the slot.
func TestRegression_PartialBackTicksAttributeToReleasedBin(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "BPU-PARTIAL",
		PayloadCode: "PART-PB",
		UOPCapacity: 100,
		InitialUOP:  50,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	const binID int64 = 11002
	const orderUUID = "uuid-bpu-partial"
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "BPU-PARTIAL-NODE", "", "", "", false, "PART-PB")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)

	// Phase 1: 2 ticks during the pickup window — both must attribute
	// to the released bin.
	for i := 0; i < 2; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: 4,
		}})
	}
	for _, c := range sink.binCalls {
		if c.BinID != binID {
			t.Errorf("during-window tick attributed to bin=%d, want %d (released bin)", c.BinID, binID)
		}
	}
	preCalls := len(sink.binCalls)
	if preCalls != 2 {
		t.Fatalf("during-window bin calls = %d, want 2", preCalls)
	}

	// Phase 2: BinPickedUp fires.
	eng.HandleBinPickedUp(orderUUID, binID)

	// Phase 3: a tick after pickup — runtime ActiveOrderID is nil now
	// so binAtNode returns 0 and no bin delta fires (bucket-only or
	// no-op). Critically, NO additional call against the released bin.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 3,
	}})

	postCalls := len(sink.binCalls) - preCalls
	for _, c := range sink.binCalls[preCalls:] {
		if c.BinID == binID {
			t.Errorf("post-pickup tick attributed to released bin=%d (must not — pickup cleared ActiveOrderID)",
				c.BinID)
		}
	}
	// Acceptable shapes: 0 bin calls (no active order → no bin delta)
	// or calls to a different bin if the runtime advanced. The
	// invariant we're pinning is "no more calls to binID" — pre-Item-11
	// each post-pickup tick would have continued to attribute to the
	// released bin.
	_ = postCalls
}
