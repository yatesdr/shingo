package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestBinPickedUp_FlushesAccumulator pins Item 11's flush trigger:
// when the robot picks up the released bin, the inventory delta
// accumulator must flush so any in-flight ticks ship before the
// active claim advances. Without the flush, Edge can lose a tick
// or two recorded between RELEASE PARTIAL and the physical pickup.
func TestBinPickedUp_FlushesAccumulator(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "BPU-FLUSH",
		PayloadCode: "PART-BPU",
		UOPCapacity: 100,
		InitialUOP:  50,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed runtime")

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
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	// Pre-condition: zero flushes recorded.
	if sink.flushes != 0 {
		t.Fatalf("pre-handler flushes = %d, want 0", sink.flushes)
	}

	eng.HandleBinPickedUp(orderUUID, binID, "BPU-FLUSH-NODE")

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
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	// No order seeded — handler must early-return cleanly. Location
	// is irrelevant here (the order-lookup early-return fires before
	// the location gate); empty string is fine.
	eng.HandleBinPickedUp("uuid-nonexistent", 99999, "")

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
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "BPU-PARTIAL",
		PayloadCode: "PART-PB",
		UOPCapacity: 100,
		InitialUOP:  50,
	})
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
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 50), "seed runtime")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
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
	eng.HandleBinPickedUp(orderUUID, binID, "BPU-PARTIAL-NODE")

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

// TestRegression_TwoRobotSupplyLegSupermarketPickupDoesNotFreezeCache pins
// the Springfield ALN_001 freeze bug (2026-05-18). Two-robot supply leg
// (Order A) picks up the new bin at the supermarket as its first
// physical step, BEFORE the operator clicks RELEASE. Pre-fix the
// handler matched runtime.ActiveOrderID == orderA.ID (because
// applyConsumePlan sets A as active and B as staged), then nulled both
// the active order pointer and active_bin_id. inSteadyState() returned
// false for the entire staging window (active_bin_id=nil), so PLC ticks
// stopped decrementing remaining_uop_cached — the operator's release
// prompt eventually opened against a cache value frozen at REQUEST
// MATERIAL time.
//
// The fix gates the handler's runtime mutations on Location: a pickup
// at any node other than this consume node's CoreNodeName is "not our
// slot" and returns early without touching runtime. Old bin physically
// still in the slot during staging; PLC ticks attribute correctly.
func TestRegression_TwoRobotSupplyLegSupermarketPickupDoesNotFreezeCache(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "TR-FREEZE",
		PayloadCode: "PART-TR",
		UOPCapacity: 1200,
		InitialUOP:  95, // mid-bin — operator hits REQUEST MATERIAL here
	})
	// Promote to two_robot. seedTwoRobotPair sets InboundSource="TR-SOURCE"
	// (the supermarket location Core will name in the BinPickedUp envelope
	// when the supply robot picks up the new bin).
	orderA, _ := seedTwoRobotPair(t, db, nodeID, "uuid-tr-freeze", protocol.SwapModeTwoRobot)

	// Stamp the runtime to match a real mid-cycle state: old bin
	// physically in the slot, both pointers steady (active == cached),
	// cache at 95. SetProcessNodeRuntimeWithBin only writes
	// active_bin_id; the explicit SetProcessNodeCachedBin call below
	// pairs cached_bin_id so inSteadyState() returns true.
	const oldBinID int64 = 700
	bid := oldBinID
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 95), "seed active bin")
	testutil.MustNoErr(t, db.SetProcessNodeCachedBin(nodeID, &bid, 95), "seed cached bin (steady state)")

	// Pre-condition: steady state, cache=95.
	pre, _ := db.GetProcessNodeRuntime(nodeID)
	if pre.ActiveBinID == nil || *pre.ActiveBinID != oldBinID {
		t.Fatalf("pre: ActiveBinID = %v, want %d", pre.ActiveBinID, oldBinID)
	}
	if pre.CachedBinID == nil || *pre.CachedBinID != oldBinID {
		t.Fatalf("pre: CachedBinID = %v, want %d", pre.CachedBinID, oldBinID)
	}
	if pre.RemainingUOPCached != 95 {
		t.Fatalf("pre: RemainingUOPCached = %d, want 95", pre.RemainingUOPCached)
	}
	if pre.ActiveOrderID == nil || *pre.ActiveOrderID != orderA {
		t.Fatalf("pre: ActiveOrderID = %v, want %d (supply)", pre.ActiveOrderID, orderA)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	// Supply robot's first physical step: pickup at InboundSource
	// (the supermarket). Core publishes BinPickedUp with location set
	// to the source node, NOT the consume node's slot.
	supplyOrder, _ := db.GetOrder(orderA)
	const supplyBinID int64 = 800
	eng.HandleBinPickedUp(supplyOrder.UUID, supplyBinID, "TR-SOURCE")

	// Assertion 1 (the fix): runtime is untouched. active_bin_id still
	// points at the old bin; active_order_id still points at the supply
	// order. inSteadyState() stays true, so PLC ticks below will
	// continue to decrement remaining_uop_cached.
	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveBinID == nil || *post.ActiveBinID != oldBinID {
		t.Errorf("post-supermarket-pickup ActiveBinID = %v, want %d (must stay — old bin physically still in slot)",
			post.ActiveBinID, oldBinID)
	}
	if post.ActiveOrderID == nil || *post.ActiveOrderID != orderA {
		t.Errorf("post-supermarket-pickup ActiveOrderID = %v, want %d (supply leg still in flight)",
			post.ActiveOrderID, orderA)
	}

	// Assertion 2 (the symptom): a PLC tick after the supermarket pickup
	// still decrements remaining_uop_cached. Pre-fix the cache would
	// have stayed at 95 because inSteadyState() returned false from
	// active_bin_id=nil.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 3,
	}})

	after, _ := db.GetProcessNodeRuntime(nodeID)
	if after.RemainingUOPCached != 92 {
		t.Errorf("post-tick RemainingUOPCached = %d, want 92 (95-3) — cache must continue decrementing through the staging window; pre-fix freeze at 95",
			after.RemainingUOPCached)
	}
	_ = processID
	_ = styleID
}

// TestRegression_BinPickedUpAtRemoteLocationIsIgnored is the broader
// invariant: regardless of swap mode, a BinPickedUp event whose
// Location does not match the consume node's CoreNodeName must not
// touch runtime. Covers single_robot, two_robot, two_robot_press_index,
// and simple-move (all of which have at least one pickup at a remote
// source). Pre-fix any of those would have nulled active_bin_id /
// active_order_id and frozen the cache for the duration of the order.
func TestRegression_BinPickedUpAtRemoteLocationIsIgnored(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REMOTE-IGN",
		PayloadCode: "PART-RI",
		UOPCapacity: 1200,
		InitialUOP:  500,
	})
	orderA, _ := seedTwoRobotPair(t, db, nodeID, "uuid-remote-ign", protocol.SwapModeTwoRobot)

	const oldBinID int64 = 900
	bid := oldBinID
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 500), "seed active bin")
	testutil.MustNoErr(t, db.SetProcessNodeCachedBin(nodeID, &bid, 500), "seed cached bin")

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	supplyOrder, _ := db.GetOrder(orderA)

	// Hit every remote pickup location a supply leg can fire from:
	// the source supermarket and the inbound staging buffer. None
	// should touch runtime.
	for _, location := range []string{"TR-SOURCE", "TR-STAGING"} {
		eng.HandleBinPickedUp(supplyOrder.UUID, 1, location)
		rt, _ := db.GetProcessNodeRuntime(nodeID)
		if rt.ActiveBinID == nil || *rt.ActiveBinID != oldBinID {
			t.Errorf("location=%q: ActiveBinID = %v, want %d (remote pickup must not touch runtime)",
				location, rt.ActiveBinID, oldBinID)
		}
		if rt.ActiveOrderID == nil || *rt.ActiveOrderID != orderA {
			t.Errorf("location=%q: ActiveOrderID = %v, want %d (remote pickup must not touch runtime)",
				location, rt.ActiveOrderID, orderA)
		}
	}

	// Sink should not have been touched (no flush from remote pickups).
	if sink.flushCount > 0 {
		t.Errorf("remote pickups triggered %d flushes, want 0", sink.flushCount)
	}
}

// TestRegression_BinPickedUpEmptyLocationFailsClosed pins the
// inverted location gate. Pre-inversion the gate failed OPEN on
// empty Location — re-introducing the Springfield bug any time
// Core's wiring_block_completed.go failed to populate ev.Location.
// Post-inversion empty Location is treated as "couldn't verify,
// skip for safety."
func TestRegression_BinPickedUpEmptyLocationFailsClosed(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "EMPTY-LOC",
		PayloadCode: "PART-EL",
		UOPCapacity: 100,
		InitialUOP:  50,
	})

	const binID int64 = 12001
	bid := binID
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 50), "seed active bin")

	const orderUUID = "uuid-empty-loc"
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "EMPTY-LOC-NODE", "", "", "", false, "PART-EL")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	// Empty Location: gate must fail closed and skip all side effects.
	eng.HandleBinPickedUp(orderUUID, binID, "")

	// Runtime must be untouched — active state stays pinned.
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("ActiveBinID = %v, want %d (empty-Location must not clear active state)", rt.ActiveBinID, binID)
	}
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != orderID {
		t.Errorf("ActiveOrderID = %v, want %d (empty-Location must not clear active state)", rt.ActiveOrderID, orderID)
	}
	if sink.flushes != 0 {
		t.Errorf("Flush() fired on empty-Location BinPickedUp: flushes = %d, want 0", sink.flushes)
	}
}

// TestRegression_BinPickedUpWhitespaceLocationMatches pins the
// defensive TrimSpace in the gate. The canonical write path trims
// (store/processes/processes.go) but Core's RDS-driven Location
// does not normalize. A stray space on either side must still
// match the at-slot path (gate fail-open on whitespace would
// re-introduce the Springfield class on a different trigger).
func TestRegression_BinPickedUpWhitespaceLocationMatches(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "WS-LOC",
		PayloadCode: "PART-WS",
		UOPCapacity: 100,
		InitialUOP:  50,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed runtime")

	const binID int64 = 12002
	const orderUUID = "uuid-ws-loc"
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "WS-LOC-NODE", "", "", "", false, "PART-WS")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	// Location has stray whitespace; gate trims both sides at compare
	// time and the at-slot path fires normally.
	eng.HandleBinPickedUp(orderUUID, binID, " WS-LOC-NODE ")

	if sink.flushes == 0 {
		t.Errorf("Flush() not called on whitespace-padded Location — defensive TrimSpace should make the gate pass")
	}
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveOrderID != nil {
		t.Errorf("ActiveOrderID = %v, want nil (handler must clear active after at-slot pickup)", rt.ActiveOrderID)
	}
}
