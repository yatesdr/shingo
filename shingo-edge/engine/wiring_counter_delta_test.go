package engine

import (
	"sync"
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
)

// fakeDeltaSink captures every Record* call so tests can assert on the
// delta stream that the PLC tick path produces. Concurrency-safe — the
// tick path is single-goroutine in tests but the production reporter
// hits sync.Map under contention.
type fakeDeltaSink struct {
	mu          sync.Mutex
	binCalls    []fakeBinCall
	bucketCalls []fakeBucketCall

	// pendingBins / pendingBuckets let tests pre-stage which scopes
	// the reconciler should treat as in-flight. Leaving them nil
	// (default) means IsPending* returns false for everything —
	// matches pre-Item-2 behavior.
	pendingBins    map[int64]struct{}
	pendingBuckets map[fakePendingBucketKey]struct{}

	// flushFailures is a configurable stand-in for the production
	// reporter's atomic counter — tests that exercise the metrics
	// surface stuff a value here.
	flushFailures int64
}

type fakeBinCall struct {
	BinID       int64
	PayloadCode string
	Delta       int
	Reason      protocol.BinUOPDeltaReason
}

type fakeBucketCall struct {
	NodeID     int64
	PairKey    string
	StyleID    int64
	PartNumber string
	Delta      int
	Reason     protocol.LinesideBucketDeltaReason
}

func (s *fakeDeltaSink) RecordBin(binID int64, payloadCode string, delta int, reason protocol.BinUOPDeltaReason) {
	s.mu.Lock()
	s.binCalls = append(s.binCalls, fakeBinCall{binID, payloadCode, delta, reason})
	s.mu.Unlock()
}

func (s *fakeDeltaSink) RecordBucket(nodeID int64, pairKey string, styleID int64, partNumber string, delta int, reason protocol.LinesideBucketDeltaReason) {
	s.mu.Lock()
	s.bucketCalls = append(s.bucketCalls, fakeBucketCall{nodeID, pairKey, styleID, partNumber, delta, reason})
	s.mu.Unlock()
}

func (s *fakeDeltaSink) Flush() {}

// IsPendingBinDelta / IsPendingBucketDelta return whatever the test
// stuffs into pendingBins / pendingBuckets. Tests that don't care
// about the pending-delta guard leave both nil and the methods return
// false (the safe default — treat nothing as pending so reconciler
// heals proceed unblocked, matching pre-Item-2 behavior).
func (s *fakeDeltaSink) IsPendingBinDelta(binID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pendingBins[binID]
	return ok
}

func (s *fakeDeltaSink) IsPendingBucketDelta(nodeID, styleID int64, partNumber string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.pendingBuckets {
		if k.nodeID == nodeID && k.styleID == styleID && k.partNumber == partNumber {
			return true
		}
	}
	return false
}

// FlushFailures lets tests stuff a configured count via the
// flushFailures field on fakeDeltaSink — defaults to 0, matching the
// production no-failures steady state.
func (s *fakeDeltaSink) FlushFailures() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushFailures
}

// fakePendingBucketKey is the test-side equivalent of
// messaging.bucketScopeKey's parsed shape — matches the reconciler's
// query signature (no pairKey).
type fakePendingBucketKey struct {
	nodeID     int64
	styleID    int64
	partNumber string
}

// TestRegression_RuntimeUOPGoesNegativeOnOverpack pins the Item 5.6
// signed-bin semantic: the consume tick path no longer clamps the
// runtime cache at zero. A real bin nominally rated N can overpack
// to N+k (operator runs an extra cycle); the runtime must reflect
// that overpack as a negative count rather than pretending the bin
// is exactly empty. Without this fix Core's authoritative count
// would diverge from Edge's clamped cache, and the reconciler would
// ping-pong forever (heal Edge negative → next tick clamps to 0 →
// reconciler heals negative again).
//
// The auto-reorder gate keeps its > 0 guard intentionally — the
// reorder fires on the threshold cross from above; subsequent ticks
// past zero must not refire (the reorder is already in flight).
// See TestRegression_RuntimeUOPNegativeNoReorderRefire.
func TestRegression_RuntimeUOPGoesNegativeOnOverpack(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "OVERPACK",
		PayloadCode: "PART-OP",
		UOPCapacity: 100,
		InitialUOP:  3,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 3); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Active order with bin id — required for the bin delta path
	// (binAtNode looks the bin up via runtime.ActiveOrderID).
	const binID int64 = 9101
	orderID, err := db.CreateOrder("uuid-overpack", orders.TypeRetrieve,
		&nodeID, false, 1, "OVERPACK-NODE", "", "", "", false, "PART-OP")
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

	// One overpack tick of 8 against a runtime of 3 → -5.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 8,
	}})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != -5 {
		t.Errorf("runtime.RemainingUOPCached = %d, want -5 (3 - 8; signed semantic, no clamp at 0)",
			rt.RemainingUOPCached)
	}

	// The bin delta must mirror the full -8 (Core's authoritative
	// count needs the full debit; signed cache and signed Core stay
	// in lockstep that way).
	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	if sink.binCalls[0].Delta != -8 {
		t.Errorf("bin delta = %d, want -8", sink.binCalls[0].Delta)
	}
}

// Reconciler ping-pong test removed alongside the reconciler deletion
// (bin-ownership flip). With no Core→Edge heal path, there is no loop
// to test against. The signed-cache invariant is still pinned by
// TestRegression_NegativeRuntimeFromOverpack above.

// TestRegression_DrainLinesideAttribution pins the Phase 1 invariant:
// when a consume tick fires against a node that has a non-empty
// lineside bucket, the tick splits between a LinesideBucketDelta
// (consume_drain) and a BinUOPDelta (consume_tick). Without this
// split the bucket vs bin attribution is implicit and Phase 2's
// reconciler can't distinguish "bucket drained" from "bin drained".
func TestRegression_DrainLinesideAttribution(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "DRAIN-ATTR",
		PayloadCode: "PART-DRAIN",
		UOPCapacity: 100,
		InitialUOP:  100,
	})

	// Seed an active order with a BinID, and pin active_bin_id to the
	// same value — bin attribution reads from the runtime row directly.
	const binID int64 = 777
	orderID, err := db.CreateOrder("uuid-drain-attr", orders.TypeRetrieve,
		&nodeID, false, 1, "DRAIN-ATTR-NODE", "", "", "", false, "PART-DRAIN")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	if err := db.UpdateOrderBinID(orderID, &bid); err != nil {
		t.Fatalf("set order bin id: %v", err)
	}
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil); err != nil {
		t.Fatalf("set runtime orders: %v", err)
	}
	if err := db.SetProcessNodeActiveBinID(nodeID, &bid); err != nil {
		t.Fatalf("set active_bin_id: %v", err)
	}

	// Seed a lineside bucket with 7 parts. A delta of 10 should drain
	// 7 from the bucket and 3 from the bin.
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-DRAIN", 7); err != nil {
		t.Fatalf("capture bucket: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)

	eng.Events.Emit(Event{
		Type: EventCounterDelta,
		Payload: CounterDeltaEvent{
			ProcessID: processID,
			StyleID:   styleID,
			Delta:     10,
		},
	})

	// Bucket: one consume_drain call for 7.
	if len(sink.bucketCalls) != 1 {
		t.Fatalf("bucket calls = %d, want 1: %+v", len(sink.bucketCalls), sink.bucketCalls)
	}
	bc := sink.bucketCalls[0]
	if bc.NodeID != nodeID || bc.StyleID != styleID || bc.PartNumber != "PART-DRAIN" {
		t.Errorf("bucket call routing mismatch: %+v (node=%d style=%d)", bc, nodeID, styleID)
	}
	if bc.Delta != -7 {
		t.Errorf("bucket delta = %d, want -7 (bucket had 7, drained all)", bc.Delta)
	}
	if bc.Reason != protocol.ReasonConsumeDrain {
		t.Errorf("bucket reason = %q, want %q", bc.Reason, protocol.ReasonConsumeDrain)
	}

	// Bin: one consume_tick call for the 3 remainder.
	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	binCall := sink.binCalls[0]
	if binCall.BinID != binID {
		t.Errorf("bin call BinID = %d, want %d", binCall.BinID, binID)
	}
	if binCall.PayloadCode != "PART-DRAIN" {
		t.Errorf("bin call PayloadCode = %q, want %q", binCall.PayloadCode, "PART-DRAIN")
	}
	if binCall.Delta != -3 {
		t.Errorf("bin call delta = %d, want -3 (10 tick - 7 bucket)", binCall.Delta)
	}
	if binCall.Reason != protocol.ReasonConsumeTick {
		t.Errorf("bin call reason = %q, want %q", binCall.Reason, protocol.ReasonConsumeTick)
	}
}

// TestRegression_NoBucketAllToBin pins the no-bucket case: a consume
// tick against a node with no active bucket sends the entire delta to
// the bin via a single BinUOPDelta(consume_tick). No bucket delta
// fires because nothing drained.
func TestRegression_NoBucketAllToBin(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "NO-BUCKET",
		PayloadCode: "PART-NB",
		UOPCapacity: 100,
		InitialUOP:  100,
	})

	const binID int64 = 888
	orderID, err := db.CreateOrder("uuid-no-bucket", orders.TypeRetrieve,
		&nodeID, false, 1, "NO-BUCKET-NODE", "", "", "", false, "PART-NB")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
	_ = db.SetProcessNodeActiveBinID(nodeID, &bid)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)

	eng.Events.Emit(Event{
		Type: EventCounterDelta,
		Payload: CounterDeltaEvent{
			ProcessID: processID,
			StyleID:   styleID,
			Delta:     5,
		},
	})

	if len(sink.bucketCalls) != 0 {
		t.Errorf("bucket calls = %d, want 0 (no bucket existed): %+v",
			len(sink.bucketCalls), sink.bucketCalls)
	}
	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	if got := sink.binCalls[0].Delta; got != -5 {
		t.Errorf("bin delta = %d, want -5", got)
	}
}

// TestRegression_NoSinkNoEmissionDoesNotPanic pins the nil-sink
// invariant — every emission site must nil-guard so engines without a
// reporter (test contexts, off-modes) don't crash on tick events.
func TestRegression_NoSinkNoEmissionDoesNotPanic(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "NO-SINK",
		PayloadCode: "PART-NS",
		UOPCapacity: 100,
		InitialUOP:  100,
	})

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	// Deliberately do NOT call SetInventoryDeltaSink.

	eng.Events.Emit(Event{
		Type: EventCounterDelta,
		Payload: CounterDeltaEvent{
			ProcessID: processID,
			StyleID:   styleID,
			Delta:     1,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 99 {
		t.Errorf("RemainingUOP = %d, want 99 (tick still applied via direct write)",
			runtime.RemainingUOPCached)
	}
}

// TestRegression_BinAttributionRequiresActiveBinID pins the
// no-bin-at-slot case: when the runtime has no active_bin_id (slot
// physically empty, or bootstrap before first delivery completes),
// consume ticks must skip the bin delta. The runtime cache still
// decrements locally — that's harmless drift on an idle slot — but
// nothing ships to Core because there's no bin to attribute to.
func TestRegression_BinAttributionRequiresActiveBinID(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "NO-BIN-ID",
		PayloadCode: "PART-NBI",
		UOPCapacity: 100,
		InitialUOP:  100,
	})

	// Active order — but explicitly clear active_bin_id (the seed
	// helper sets a default). This models a delivered order whose
	// completion hasn't anchored the bin pointer yet.
	orderID, err := db.CreateOrder("uuid-no-bin-id", orders.TypeRetrieve,
		&nodeID, false, 1, "NO-BIN-ID-NODE", "", "", "", false, "PART-NBI")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
	_ = db.SetProcessNodeActiveBinID(nodeID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)

	eng.Events.Emit(Event{
		Type: EventCounterDelta,
		Payload: CounterDeltaEvent{
			ProcessID: processID,
			StyleID:   styleID,
			Delta:     2,
		},
	})

	if len(sink.binCalls) != 0 {
		t.Errorf("bin calls = %d, want 0 (no active_bin_id, must skip): %+v",
			len(sink.binCalls), sink.binCalls)
	}
}
