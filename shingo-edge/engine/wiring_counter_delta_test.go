package engine

import (
	"log"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/lineside"
	"shingoedge/uop"
)

// fakeDeltaSink captures every emission so tests can assert on the stream the
// PLC tick path and the release path produce. Concurrency-safe — the tick path
// is single-goroutine in tests but the production accumulator hits sync.Map
// under contention.
type fakeDeltaSink struct {
	mu          sync.Mutex
	binCalls    []fakeBinCall
	bucketCalls []fakeBucketCall

	// flushCount counts Flush + MarkAttributionBoundary invocations.
	// boundaryCalls records the nodeIDs MarkAttributionBoundary was
	// called with so tests can assert FlipABNode flushed before
	// SetActivePull.
	flushCount    int
	boundaryCalls []int64

	// bindCalls / clearActiveCalls record BindActiveBin and
	// ClearActiveBin invocations for tests that assert on slot-pointer
	// lifecycle.
	bindCalls                []fakeBindCall
	clearActiveCalls         []int64
	clearActiveAndResetCalls []fakeClearActiveAndResetCall
	setClaimAndCountCalls    []fakeSetClaimAndCountCall
	//nolint:unused // asserted by the clear-route epoch tests
	setClaimCountAndEpochCalls []fakeSetClaimCountAndEpochCall
	onDeliveredCalls           []fakeOnDeliveredCall
	manualLoadCalls            []fakeManualLoadCall
	onBinPickedUpCalls         []*int64
	captureToLinesideCalls     []uop.CaptureEvent

	// db (optional) — when set, the slot verbs and the capture also
	// perform the underlying write via *store.DB so tests asserting on
	// post-state see the side effect. Tests that only care about the
	// recorded calls can leave this nil. Untyped so this test file doesn't
	// need to import store; tests construct with the concrete *store.DB
	// which satisfies writeActiveBinIDer via duck typing.
	db writeActiveBinIDer
}

// writeActiveBinIDer is the fake's narrow view of *store.DB — just the
// methods the verb implementations delegate to. Defined locally so this
// test file doesn't take a hard dep on store/processes. *store.DB
// satisfies it.
type writeActiveBinIDer interface {
	SetProcessNodeActiveBinID(processNodeID int64, activeBinID *int64) error
	ClearProcessNodeActiveBinAndCount(processNodeID int64) error
	SetProcessNodeActiveBinIDAndEpoch(processNodeID int64, activeBinID *int64, deltaEpoch int64) error
	SetProcessNodeRuntimeWithBin(processNodeID int64, activeClaimID, activeBinID *int64, remainingUOP int) error
	SetProcessNodeRuntimeWithBinAndEpoch(processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOP int) error
	SetProcessNodeRuntime(processNodeID int64, activeClaimID *int64, remainingUOP int) error
	SetProcessNodeRuntimeClaimCountAndEpoch(processNodeID int64, activeClaimID *int64, remainingUOP int, binID, deltaEpoch int64) error
	SetProcessNodeRuntimeForDeliveredBin(processNodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, remainingUOP int) error
	CaptureLinesideBucket(nodeID int64, payloadCode string, qty int) (int, error)
}

type fakeBinCall struct {
	BinID       int64
	PayloadCode string
	Delta       int
	Reason      protocol.BinUOPDeltaReason
	Epoch       int64
}

// fakeBucketCall is one pile level marked dirty: the key, and the drain the
// mark carried (0 for anything but a consume drain).
type fakeBucketCall struct {
	NodeID       int64
	CoreNodeName string
	PayloadCode  string
	State        string
	Drained      int
}

// WithPending runs fn; the fake holds no unflushed counts.
func (s *fakeDeltaSink) WithPending(fn func(uop.Pending) error) error {
	return fn(uop.Pending{})
}

func (s *fakeDeltaSink) Flush() {
	s.mu.Lock()
	s.flushCount++
	s.mu.Unlock()
}

// MarkAttributionBoundary records the boundary-flush call. Tests that
// want to assert FlipABNode flushed before SetActivePull can read
// boundaryCalls.
func (s *fakeDeltaSink) MarkAttributionBoundary(nodeID int64) error {
	s.mu.Lock()
	s.boundaryCalls = append(s.boundaryCalls, nodeID)
	s.flushCount++
	s.mu.Unlock()
	return nil
}

// BindActiveBin / ClearActiveBin record the pointer writes. Tests
// asserting on slot lifecycle can read bindCalls / clearActiveCalls
// to verify which (nodeID, binID) pairs were bound or cleared.
type fakeBindCall struct {
	NodeID int64
	BinID  int64
	Epoch  int64
}

func (s *fakeDeltaSink) BindActiveBin(nodeID, binID int64, deltaEpoch int64) error {
	s.mu.Lock()
	s.bindCalls = append(s.bindCalls, fakeBindCall{nodeID, binID, deltaEpoch})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeActiveBinIDAndEpoch(nodeID, &binID, deltaEpoch)
	}
	return nil
}

func (s *fakeDeltaSink) ClearActiveBin(nodeID int64) error {
	s.mu.Lock()
	s.clearActiveCalls = append(s.clearActiveCalls, nodeID)
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.ClearProcessNodeActiveBinAndCount(nodeID)
	}
	return nil
}

type fakeClearActiveAndResetCall struct {
	NodeID  int64
	ClaimID *int64
}

func (s *fakeDeltaSink) ClearActiveAndReset(nodeID int64, activeClaimID *int64) error {
	s.mu.Lock()
	s.clearActiveAndResetCalls = append(s.clearActiveAndResetCalls, fakeClearActiveAndResetCall{nodeID, activeClaimID})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntimeWithBin(nodeID, activeClaimID, nil, 0)
	}
	return nil
}

type fakeSetClaimAndCountCall struct {
	NodeID  int64
	ClaimID *int64
	UOP     int
}

func (s *fakeDeltaSink) SetClaimAndCount(nodeID int64, activeClaimID *int64, uop int) error {
	s.mu.Lock()
	s.setClaimAndCountCalls = append(s.setClaimAndCountCalls, fakeSetClaimAndCountCall{nodeID, activeClaimID, uop})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntime(nodeID, activeClaimID, uop)
	}
	return nil
}

type fakeSetClaimCountAndEpochCall struct {
	NodeID  int64
	ClaimID *int64
	UOP     int
	BinID   int64
	Epoch   int64
}

func (s *fakeDeltaSink) SetClaimCountAndEpoch(nodeID int64, activeClaimID *int64, uop int, binID, deltaEpoch int64) error {
	s.mu.Lock()
	s.setClaimCountAndEpochCalls = append(s.setClaimCountAndEpochCalls,
		fakeSetClaimCountAndEpochCall{nodeID, activeClaimID, uop, binID, deltaEpoch})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntimeClaimCountAndEpoch(nodeID, activeClaimID, uop, binID, deltaEpoch)
	}
	return nil
}

type fakeOnDeliveredCall struct {
	NodeID  int64
	ClaimID *int64
	BinID   int64
	Epoch   int64
	UOP     int
}

func (s *fakeDeltaSink) OnDelivered(nodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, uop int) error {
	s.mu.Lock()
	s.onDeliveredCalls = append(s.onDeliveredCalls, fakeOnDeliveredCall{nodeID, activeClaimID, binID, deltaEpoch, uop})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntimeForDeliveredBin(nodeID, activeClaimID, binID, deltaEpoch, uop)
	}
	return nil
}

type fakeManualLoadCall struct {
	NodeID  int64
	ClaimID *int64
	BinID   *int64
	Epoch   int64
	UOP     int
}

func (s *fakeDeltaSink) ManualLoad(nodeID int64, activeClaimID *int64, binID *int64, deltaEpoch int64, uop int) error {
	s.mu.Lock()
	s.manualLoadCalls = append(s.manualLoadCalls, fakeManualLoadCall{nodeID, activeClaimID, binID, deltaEpoch, uop})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, activeClaimID, binID, deltaEpoch, uop)
	}
	return nil
}

func (s *fakeDeltaSink) OnBinPickedUp(nodeID *int64) error {
	s.mu.Lock()
	s.onBinPickedUpCalls = append(s.onBinPickedUpCalls, nodeID)
	s.flushCount++
	s.mu.Unlock()
	return nil
}

// CaptureToLineside records the call and mirrors the real verb: nothing on
// the supply leg; otherwise, when db is set, the pile writes through the real
// *store.DB, a dirty mark per part, and the bin's capture_reduction.
func (s *fakeDeltaSink) CaptureToLineside(ev uop.CaptureEvent) (int, error) {
	s.mu.Lock()
	s.captureToLinesideCalls = append(s.captureToLinesideCalls, ev)
	db := s.db
	s.mu.Unlock()
	if ev.SuppressBinDelta || ev.Disposition.Mode != uop.DispositionCaptureLineside {
		return 0, nil
	}
	capturedTotal := 0
	for part, qty := range ev.Disposition.LinesideCapture {
		if qty <= 0 || part == "" {
			continue
		}
		if db != nil {
			if _, err := db.CaptureLinesideBucket(ev.NodeID, part, qty); err != nil {
				return capturedTotal, err
			}
		}
		s.mu.Lock()
		s.bucketCalls = append(s.bucketCalls, fakeBucketCall{ev.NodeID, ev.CoreNodeName, part, lineside.StateActive, 0})
		s.mu.Unlock()
		capturedTotal += qty
	}
	if capturedTotal > 0 {
		if ev.BinID > 0 {
			s.mu.Lock()
			s.binCalls = append(s.binCalls, fakeBinCall{ev.BinID, ev.PayloadCode, -capturedTotal, protocol.ReasonCaptureReduction, ev.BinEpoch})
			s.mu.Unlock()
		} else {
			// Mirror the real verb's loud diagnostic when the caller
			// couldn't resolve a bin id.
			log.Printf("ERROR: uop capture: capture_reduction skipped (BinID=0) node=%d payload=%q captured_total=%d disposition=%q",
				ev.NodeID, ev.PayloadCode, capturedTotal, ev.Disposition.Mode)
		}
	}
	return capturedTotal, nil
}

// Consumed / Produced / Fallthrough append what the real verbs record: a
// dirty mark with its drain per drained pile, and the bin delta.
func (s *fakeDeltaSink) Consumed(ev uop.TickEvent) error {
	s.recordTick(ev, protocol.ReasonConsumeTick)
	return nil
}

func (s *fakeDeltaSink) Produced(ev uop.TickEvent) error {
	s.mu.Lock()
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		s.binCalls = append(s.binCalls, fakeBinCall{ev.BinID, ev.PayloadCode, ev.BinRemainder, protocol.ReasonProduceTick, ev.BinEpoch})
	}
	s.mu.Unlock()
	return nil
}

func (s *fakeDeltaSink) Fallthrough(ev uop.TickEvent) error {
	s.recordTick(ev, protocol.ReasonABFallthrough)
	return nil
}

func (s *fakeDeltaSink) recordTick(ev uop.TickEvent, binReason protocol.BinUOPDeltaReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for part, qty := range ev.Drains {
		if qty > 0 {
			s.bucketCalls = append(s.bucketCalls, fakeBucketCall{ev.NodeID, ev.CoreNodeName, part, lineside.StateActive, qty})
		}
	}
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		s.binCalls = append(s.binCalls, fakeBinCall{ev.BinID, ev.PayloadCode, -ev.BinRemainder, binReason, ev.BinEpoch})
	}
}

// PilesChanged records a dirty mark per key and a flush, as the real verb.
func (s *fakeDeltaSink) PilesChanged(keys ...lineside.Key) {
	s.mu.Lock()
	for _, k := range keys {
		s.bucketCalls = append(s.bucketCalls, fakeBucketCall{k.NodeID, k.CoreNodeName, k.PayloadCode, k.State, 0})
	}
	s.flushCount++
	s.mu.Unlock()
}

// ResendLevels is the boot resend; the fake has nothing to resend.
func (s *fakeDeltaSink) ResendLevels() (int, error) { return 0, nil }

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
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "OVERPACK",
		PayloadCode: "PART-OP",
		UOPCapacity: 100,
		InitialUOP:  3,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 3), "seed runtime")

	// Active order with bin id — required for the bin delta path
	// (binAtNode looks the bin up via runtime.ActiveOrderID).
	const binID int64 = 9101
	orderID, err := db.CreateOrder("uuid-overpack", orders.TypeRetrieve,
		&nodeID, false, 1, "OVERPACK-NODE", "", "", "", false, "PART-OP", "", "")
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

	rtRt, errRt := db.GetProcessNodeRuntime(nodeID)
	rt := testutil.Must(t, rtRt, errRt, "load node runtime")
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
// lineside pile, the tick splits between the pile (its level, with the
// drain counted for Core's drain ledger) and a BinUOPDelta
// (consume_tick). Without this split the pile vs bin attribution is
// implicit and Core can't distinguish "pile drained" from "bin drained".
func TestRegression_DrainLinesideAttribution(t *testing.T) {
	t.Parallel()
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
		&nodeID, false, 1, "DRAIN-ATTR-NODE", "", "", "", false, "PART-DRAIN", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	testutil.MustNoErr(t, db.UpdateOrderBinID(orderID, &bid), "set order bin id")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "set runtime orders")
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bid), "set active_bin_id")

	// Seed a lineside bucket with 7 parts. A delta of 10 should drain
	// 7 from the bucket and 3 from the bin.
	if _, err := db.CaptureLinesideBucket(nodeID, "PART-DRAIN", 7); err != nil {
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

	// Pile: one dirty mark carrying the drain of 7.
	if len(sink.bucketCalls) != 1 {
		t.Fatalf("bucket calls = %d, want 1: %+v", len(sink.bucketCalls), sink.bucketCalls)
	}
	bc := sink.bucketCalls[0]
	if bc.NodeID != nodeID || bc.PayloadCode != "PART-DRAIN" || bc.State != "active" {
		t.Errorf("bucket call routing mismatch: %+v (node=%d)", bc, nodeID)
	}
	if bc.Drained != 7 {
		t.Errorf("bucket drained = %d, want 7 (bucket had 7, drained all)", bc.Drained)
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
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "NO-BUCKET",
		PayloadCode: "PART-NB",
		UOPCapacity: 100,
		InitialUOP:  100,
	})

	const binID int64 = 888
	orderID, err := db.CreateOrder("uuid-no-bucket", orders.TypeRetrieve,
		&nodeID, false, 1, "NO-BUCKET-NODE", "", "", "", false, "PART-NB", "", "")
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
	t.Parallel()
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

	runtimeRt, errRt := db.GetProcessNodeRuntime(nodeID)
	runtime := testutil.Must(t, runtimeRt, errRt, "load node runtime")
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
	t.Parallel()
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
		&nodeID, false, 1, "NO-BIN-ID-NODE", "", "", "", false, "PART-NBI", "", "")
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
