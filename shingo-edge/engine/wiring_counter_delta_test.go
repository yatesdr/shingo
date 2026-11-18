package engine

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/processes"
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
	// called with so tests can assert the release flip flushed before
	// the pull bits moved.
	flushCount    int
	boundaryCalls []int64

	// bindCalls / clearActiveCalls record BindActiveBin and
	// ClearActiveBin invocations for tests that assert on slot-pointer
	// lifecycle.
	bindCalls        []fakeBindCall
	clearActiveCalls []int64
	setClaimCalls    []fakeSetClaimCall
	//nolint:unused // asserted by the clear-route epoch tests
	setClaimCountAndEpochCalls []fakeSetClaimCountAndEpochCall
	bindFromCoreCalls          []fakeManualLoadCall
	landCalls                  []fakeLandCall
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
	ClearProcessNodeCarrier(processNodeID int64) (bool, error)
	LeaveProcessNodeAtPickup(processNodeID, pickupEpoch int64, hold int, holdPart string) error
	SetProcessNodeActiveBinIDAndEpoch(processNodeID int64, activeBinID *int64, deltaEpoch int64) error
	SetProcessNodeRuntimeWithBinAndEpoch(processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOP int) error
	SetProcessNodeRuntimeWithBinAndEpochIn(ex processes.DBTX, processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOP int) error
	SetProcessNodeActiveClaim(processNodeID int64, activeClaimID *int64) error
	SetProcessNodeRuntimeClaimCountAndEpoch(processNodeID int64, activeClaimID *int64, remainingUOP int, binID, deltaEpoch int64) error
	BindEmptySlotUnlessDeparted(processNodeID int64, activeClaimID *int64, binID, deltaEpoch int64, remainingUOP int) (bool, error)
	BindEmptySlotUnlessDepartedIn(ex processes.DBTX, processNodeID int64, activeClaimID *int64, binID, deltaEpoch int64, remainingUOP int) (bool, error)
	BindLandingAt(processNodeID int64, l processes.LandingBind) (bool, bool, error)
	BindLandingIn(ex processes.DBTX, processNodeID int64, l processes.LandingBind) (bool, bool, error)
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
// want to assert the release flip flushed before the pull bits moved can
// read boundaryCalls.
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

// ClearActiveBin with no db reports a carrier bound: the recorded-calls-only
// fixtures call the verb to say a carrier left.
func (s *fakeDeltaSink) ClearActiveBin(nodeID int64) (bool, error) {
	s.mu.Lock()
	s.clearActiveCalls = append(s.clearActiveCalls, nodeID)
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.ClearProcessNodeCarrier(nodeID)
	}
	return true, nil
}

type fakeSetClaimCall struct {
	NodeID  int64
	ClaimID *int64
}

func (s *fakeDeltaSink) SetClaim(nodeID int64, activeClaimID *int64) error {
	s.mu.Lock()
	s.setClaimCalls = append(s.setClaimCalls, fakeSetClaimCall{nodeID, activeClaimID})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeActiveClaim(nodeID, activeClaimID)
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

type fakeLandCall struct {
	NodeID int64
	Bind   processes.LandingBind
}

// Land is the landing verb (Lane H, H.4; uop.Mutator.Land), which replaced
// OnDelivered: it records the call and binds through the real statement.
func (s *fakeDeltaSink) Land(ex processes.DBTX, nodeID int64, l processes.LandingBind) (bool, bool, error) {
	s.mu.Lock()
	s.landCalls = append(s.landCalls, fakeLandCall{nodeID, l})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.BindLandingIn(ex, nodeID, l)
	}
	return true, true, nil
}

type fakeManualLoadCall struct {
	NodeID  int64
	ClaimID *int64
	BinID   *int64
	Epoch   int64
	UOP     int
}

func (s *fakeDeltaSink) ManualLoad(ex processes.DBTX, nodeID int64, activeClaimID *int64, binID *int64, deltaEpoch int64, uop int) error {
	s.mu.Lock()
	s.manualLoadCalls = append(s.manualLoadCalls, fakeManualLoadCall{nodeID, activeClaimID, binID, deltaEpoch, uop})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntimeWithBinAndEpochIn(ex, nodeID, activeClaimID, binID, deltaEpoch, uop)
	}
	return nil
}

// BindFromCore records and delegates like ManualLoad (the same statement).
func (s *fakeDeltaSink) BindFromCore(ex processes.DBTX, nodeID int64, activeClaimID *int64, binID *int64, deltaEpoch int64, uop int) error {
	s.mu.Lock()
	s.bindFromCoreCalls = append(s.bindFromCoreCalls, fakeManualLoadCall{nodeID, activeClaimID, binID, deltaEpoch, uop})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntimeWithBinAndEpochIn(ex, nodeID, activeClaimID, binID, deltaEpoch, uop)
	}
	return nil
}

// BindStagedUnlessDeparted delegates to the store's guarded bind; with no db
// it reports a bind, as the recorded-calls-only tests expect a clean path.
func (s *fakeDeltaSink) BindStagedUnlessDeparted(ex processes.DBTX, nodeID int64, activeClaimID *int64, binID, deltaEpoch int64, uop int) (bool, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.BindEmptySlotUnlessDepartedIn(ex, nodeID, activeClaimID, binID, deltaEpoch, uop)
	}
	return true, nil
}

// LeaveAtPickup records the pickup's leave as a clear (the same pointer/count
// write, Lane H D11) and runs the real statement when a DB is wired.
func (s *fakeDeltaSink) LeaveAtPickup(nodeID, pickupEpoch int64, hold int, holdPart string) error {
	s.mu.Lock()
	s.clearActiveCalls = append(s.clearActiveCalls, nodeID)
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.LeaveProcessNodeAtPickup(nodeID, pickupEpoch, hold, holdPart)
	}
	return nil
}

// TakeAfterLift: the fake holds no windows.
func (s *fakeDeltaSink) TakeAfterLift(binID, epoch int64, liftedAt time.Time) (int, error) {
	return 0, nil
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
// The tick no longer orders at all: the auto-reorder is the level
// sweep (demand_reconciler.go), which reads a negative count as
// breached like any other and asks nothing while an order already
// works the cell, so ticks past zero do not refire. That dedup is
// pinned at zero by TestLevelSweep_AsksOnceForAWedgedCell; the
// negative-count test this paragraph once named, called
// TestRegression_RuntimeUOPNegativeNoReorderRefire, was never written.
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
		&nodeID, false, 1, "OVERPACK-NODE", "", "", protocol.Height{}, false, "PART-OP", "", "")
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
// TestRegression_RuntimeUOPGoesNegativeOnOverpack above.

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
		&nodeID, false, 1, "DRAIN-ATTR-NODE", "", "", protocol.Height{}, false, "PART-DRAIN", "", "")
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
		&nodeID, false, 1, "NO-BUCKET-NODE", "", "", protocol.Height{}, false, "PART-NB", "", "")
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
		&nodeID, false, 1, "NO-BIN-ID-NODE", "", "", protocol.Height{}, false, "PART-NBI", "", "")
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

// piecesPerCycle gives part's catalog row one manifest line at ratio.
func piecesPerCycle(t *testing.T, db *store.DB, part string, ratio int64) {
	t.Helper()
	b, err := json.Marshal([]protocol.ManifestItem{{PartNumber: part, PartsPerCycle: ratio}})
	testutil.MustNoErr(t, err, "manifest")
	res, err := db.DB.Exec(`UPDATE payload_catalog SET manifest=? WHERE code=?`, string(b), part)
	testutil.MustNoErr(t, err, "manifest "+part)
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("manifest %s: %d catalog rows, want 1", part, n)
	}
}

// TestConsume_DrawsPiecesPerCycleAtEveryConsumePath is the owner's example on
// every consume path: a part used 8 per cycle, an 800-piece cart, 100 cycles
// to empty — at a consume spot the cell counter counts, at an A/B pair's
// fall-through side and at a spot a station counter counts. A produce spot of
// the same part never reads the number: it adds what its counter counted. A
// draw bigger than what is left takes the count below 0 (8 against 5 is -3),
// and nothing carries anywhere. Before, every cart ended at 700.
func TestConsume_DrawsPiecesPerCycleAtEveryConsumePath(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	b1, b2, b3, b4, b5 := int64(61), int64(62), int64(63), int64(64), int64(65)
	f := seedWalkerProcess(t, db, "PPC", []walkerNode{
		{Suffix: "CELL", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-R8", Capacity: 800, Remaining: 800, BoundBin: &b1, ActivePull: true},
		{Suffix: "AB_A", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-R8", Capacity: 800, Remaining: 800, BoundBin: &b2, Paired: "PPC_AB_B"},
		{Suffix: "AB_B", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential,
			PayloadCode: "SYN-R8", Capacity: 800, Remaining: 800, BoundBin: &b3, Paired: "PPC_AB_A"},
		{Suffix: "STATION", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-R8", Capacity: 800, Remaining: 800, BoundBin: &b4, ActivePull: true},
		{Suffix: "PRESS", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-R8", Capacity: 800, Remaining: 0, BoundBin: &b5, ActivePull: true},
	})
	piecesPerCycle(t, db, "SYN-R8", 8)
	lc := stationCounter(t, db, f.ProcessID, f.node(t, "STATION"))
	eng := testEngine(t, db)

	for range 100 {
		eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 1})
		eng.handleLineCounterDelta(LineCounterDeltaEvent{ProcessID: f.ProcessID, CounterID: lc, StyleID: f.StyleID,
			Kind: plc.TickToday, Delta: 1})
	}
	got := tickOutcomes(t, db, f)
	for suffix, want := range map[string]int{"CELL": 0, "AB_A": 0, "AB_B": 800, "STATION": 0, "PRESS": 100} {
		if got[suffix].Remaining != want {
			t.Errorf("%s after 100 cycles = %d, want %d", suffix, got[suffix].Remaining, want)
		}
	}

	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(f.node(t, "CELL"), binPtr(f.ClaimIDs["CELL"]), &b1, 5), "5 left")
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 1})
	if got := runtimeOf(t, eng, f.node(t, "CELL")).RemainingUOPCached; got != -3 {
		t.Errorf("8 against 5 = %d, want -3: the draw takes the count below 0", got)
	}
}

// TestConsume_ALedgerRowReleasesInItsOwnUnit: a held share counts strokes,
// whose part was not known when they were held, so its release draws them
// times the pieces per cycle of the cart it lands on; a "no cart" row already
// counts the pieces the cart did not give, so its release draws them as they
// are, and a Charge's preview says the same numbers.
func TestConsume_ALedgerRowReleasesInItsOwnUnit(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	b1 := int64(71)
	f := seedWalkerProcess(t, db, "LDG", []walkerNode{
		{Suffix: "CELL", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-L8", Capacity: 800, Remaining: 800, BoundBin: &b1, ActivePull: true},
		{Suffix: "NOCART", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-L8", Capacity: 800, Remaining: 0, ActivePull: true},
	})
	piecesPerCycle(t, db, "SYN-L8", 8)
	eng := testEngine(t, db)

	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 2, CATID: "9201", Kind: plc.TickHeld})
	eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 1, CATID: "9202"})
	if got := runtimeOf(t, eng, f.node(t, "CELL")).RemainingUOPCached; got != 792 {
		t.Fatalf("CELL after today's stroke = %d, want 792", got)
	}
	var held, noCart *store.CatidHold
	hs, err := db.ListOpenCatidHolds(f.ProcessID)
	testutil.MustNoErr(t, err, "ledger")
	for i := range hs {
		switch hs[i].Reason {
		case holdReasonHeld:
			held = &hs[i]
		case holdReasonNoCart:
			noCart = &hs[i]
		}
	}
	if held == nil || held.Qty != 2 || noCart == nil || noCart.Qty != 8 || noCart.Strokes != 1 {
		t.Fatalf("ledger = %+v, want a held draw of 2 strokes and a no-cart row of 8 pieces over 1 stroke", hs)
	}
	if it := noCartItem(*noCart, "NOCART", time.UTC); !strings.HasPrefix(it.Headline, "8 pieces of SYN-L8 at NOCART") {
		t.Errorf("no-cart headline = %q, want it to count the 8 pieces", it.Headline)
	}

	// A cart of the part lands at NOCART; both rows are released onto carts.
	nc := f.node(t, "NOCART")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nc, binPtr(f.ClaimIDs["NOCART"]), binPtr(72), 800), "a cart")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nc, "SYN-L8", true, "test"), "its part")

	rows, refusal, err := eng.holdRows(noCart, f.StyleID, "LDG-STYLE")
	testutil.MustNoErr(t, err, "preview no-cart")
	if refusal != "" || len(rows) != 1 || rows[0].Qty != 8 {
		t.Errorf("no-cart preview = %+v (%q), want one row drawing 8", rows, refusal)
	}
	rows, _, err = eng.holdRows(held, f.StyleID, "LDG-STYLE")
	testutil.MustNoErr(t, err, "preview held")
	for _, r := range rows {
		if r.Qty != 16 {
			t.Errorf("held preview row %+v: qty %d, want 16 (2 strokes at 8)", r, r.Qty)
		}
	}

	eng.chargeHold(noCart, f.StyleID)
	if got := runtimeOf(t, eng, nc).RemainingUOPCached; got != 792 {
		t.Errorf("NOCART after the no-cart row's release = %d, want 792: its 8 pieces drawn once", got)
	}
	eng.chargeHold(held, f.StyleID)
	if got := runtimeOf(t, eng, f.node(t, "CELL")).RemainingUOPCached; got != 776 {
		t.Errorf("CELL after the held draw's release = %d, want 776: 2 strokes at 8", got)
	}
}

// TestShareStep_SumsToTheStepFloorFirst: over every step 0-60 and 1-6 spots,
// the shares sum to the step, each is the floor share or one more, and the
// odd pieces go to the first spots in node order.
func TestShareStep_SumsToTheStepFloorFirst(t *testing.T) {
	t.Parallel()
	for n := 1; n <= 6; n++ {
		for step := 0; step <= 60; step++ {
			got := shareStep(step, n)
			if len(got) != n {
				t.Fatalf("shareStep(%d, %d) = %v: %d shares, want %d", step, n, got, len(got), n)
			}
			sum := 0
			for i, s := range got {
				sum += s
				want := step / n
				if i < step%n {
					want++
				}
				if s != want {
					t.Errorf("shareStep(%d, %d)[%d] = %d, want %d (floor %d, the first %d spots one more)",
						step, n, i, s, want, step/n, step%n)
				}
			}
			if sum != step {
				t.Errorf("shareStep(%d, %d) = %v sums to %d, want the step", step, n, got, sum)
			}
		}
	}
}

// TestProduce_TwoSpotsOfOnePartShareTheStep: two produce spots of one style
// running one part on one counter split each step, the odd piece to the first
// in node order: steps 2, 4 and 1 give A 1+2+1 and B 1+2+0. An LH/RH die's
// two parts are untouched: each takes the whole step. Before, A and B each
// took the whole step (7 and 7).
func TestProduce_TwoSpotsOfOnePartShareTheStep(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	b1, b2, b3, b4 := int64(81), int64(82), int64(83), int64(84)
	f := seedWalkerProcess(t, db, "SHR", []walkerNode{
		{Suffix: "A", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-TWO", Capacity: 100, BoundBin: &b1, ActivePull: true},
		{Suffix: "B", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-TWO", Capacity: 100, BoundBin: &b2, ActivePull: true},
		{Suffix: "LH", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-LH2", Capacity: 100, BoundBin: &b3, ActivePull: true},
		{Suffix: "RH", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: "SYN-RH2", Capacity: 100, BoundBin: &b4, ActivePull: true},
	})
	eng := testEngine(t, db)
	for _, step := range []int64{2, 4, 1} {
		eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: step})
	}
	got := tickOutcomes(t, db, f)
	for suffix, want := range map[string]int{"A": 4, "B": 3, "LH": 7, "RH": 7} {
		if got[suffix].Remaining != want {
			t.Errorf("%s after steps 2, 4, 1 = %d, want %d", suffix, got[suffix].Remaining, want)
		}
	}
}

// TestProduce_AnABPairIsOneSpot: a produce A/B pair of one part takes the
// whole step at its lit side; beside a lone spot of the same part it is one
// of two spots, so each takes half.
func TestProduce_AnABPairIsOneSpot(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name               string
		lone               bool
		wantLit, wantAlone int
	}{
		{name: "the pair alone", wantLit: 4},
		{name: "the pair beside a lone spot of its part", lone: true, wantLit: 2, wantAlone: 2},
	} {
		db := testEngineDB(t)
		b1, b2, b3 := int64(91), int64(92), int64(93)
		spots := []walkerNode{
			{Suffix: "PA", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential,
				PayloadCode: "SYN-AB2", Capacity: 100, BoundBin: &b1, Paired: "ABP_PB", ActivePull: true},
			{Suffix: "PB", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential,
				PayloadCode: "SYN-AB2", Capacity: 100, BoundBin: &b2, Paired: "ABP_PA", ActivePull: true},
		}
		if c.lone {
			spots = append(spots, walkerNode{Suffix: "ALONE", Role: protocol.ClaimRoleProduce,
				SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "SYN-AB2", Capacity: 100, BoundBin: &b3, ActivePull: true})
		}
		f := seedWalkerProcess(t, db, "ABP", spots)
		eng := testEngine(t, db)
		eng.handleCounterDelta(CounterDeltaEvent{ProcessID: f.ProcessID, StyleID: f.StyleID, Delta: 4})
		got := tickOutcomes(t, db, f)
		if got["PA"].Remaining != c.wantLit || got["PB"].Remaining != 0 {
			t.Errorf("%s: the pair = %d / %d, want %d at its first lit side and 0 at its second", c.name,
				got["PA"].Remaining, got["PB"].Remaining, c.wantLit)
		}
		if c.lone && got["ALONE"].Remaining != c.wantAlone {
			t.Errorf("%s: the lone spot = %d, want %d", c.name, got["ALONE"].Remaining, c.wantAlone)
		}
	}
}
