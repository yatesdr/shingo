package engine

import (
	"log"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/lineside"
	"shingoedge/store/processes"
	"shingoedge/uop"
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
	// matches pre-Item-2 behavior. (Pre-flip carry-over; the
	// reconciler is deleted but the test fake methods remain unused.)
	pendingBins    map[int64]struct{}
	pendingBuckets map[fakePendingBucketKey]struct{}

	// flushFailures is a configurable stand-in for the production
	// reporter's atomic counter — tests that exercise the metrics
	// surface stuff a value here.
	flushFailures int64

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
	prepareIncomingCalls     []fakePrepareIncomingCall
	clearCacheCalls          []int64
	clearActiveAndResetCalls []fakeClearActiveAndResetCall
	setClaimAndCountCalls    []fakeSetClaimAndCountCall
	onDeliveredCalls         []fakeOnDeliveredCall
	manualLoadCalls          []fakeManualLoadCall
	onBinPickedUpCalls       []*int64
	adjustBucketCalls        []fakeAdjustBucketCall
	backfillCalls            int
	captureToLinesideCalls   []uop.CaptureEvent

	// db (optional) — when set, BindActiveBin / ClearActiveBin also
	// perform the underlying runtime-row write via *store.DB so tests
	// asserting on post-state see the side effect. Tests that only
	// care about the recorded calls can leave this nil. Untyped so
	// this test file doesn't need to import store; tests construct
	// with the concrete *store.DB which satisfies the writeActiveBinIDer
	// behaviour via duck typing.
	db writeActiveBinIDer
}

// writeActiveBinIDer is the fake's narrow view of *store.DB — just the
// methods the verb implementations delegate to. Defined locally so this
// test file doesn't take a hard dep on store/processes. *store.DB
// satisfies it.
type writeActiveBinIDer interface {
	SetProcessNodeActiveBinID(processNodeID int64, activeBinID *int64) error
	SetProcessNodeCachedBin(processNodeID int64, cachedBinID *int64, remainingUOP int) error
	SetProcessNodeRuntimeWithBin(processNodeID int64, activeClaimID, activeBinID *int64, remainingUOP int) error
	SetProcessNodeRuntime(processNodeID int64, activeClaimID *int64, remainingUOP int) error
	SetProcessNodeRuntimeForDeliveredBin(processNodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, remainingUOP int) error
	SetLinesideBucketForReconcile(nodeID int64, pairKey string, styleID int64, partNumber string, qty int) error
}

type fakeBinCall struct {
	BinID       int64
	PayloadCode string
	Delta       int
	Reason      protocol.BinUOPDeltaReason
	Epoch       int64
}

type fakeBucketCall struct {
	NodeID      int64
	PairKey     string
	StyleID     int64
	PartNumber  string
	PayloadCode string
	Delta       int
	Reason      protocol.LinesideBucketDeltaReason
}

func (s *fakeDeltaSink) RecordBin(binID int64, payloadCode string, delta int, reason protocol.BinUOPDeltaReason, epoch int64) {
	s.mu.Lock()
	s.binCalls = append(s.binCalls, fakeBinCall{binID, payloadCode, delta, reason, epoch})
	s.mu.Unlock()
}

func (s *fakeDeltaSink) RecordBucket(nodeID int64, coreNodeName, pairKey string, styleID int64, partNumber, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason) {
	s.mu.Lock()
	s.bucketCalls = append(s.bucketCalls, fakeBucketCall{nodeID, pairKey, styleID, partNumber, payloadCode, delta, reason})
	_ = coreNodeName // tests assert on the legacy call shape; coreNodeName is wire-only metadata
	s.mu.Unlock()
}

func (s *fakeDeltaSink) Flush() {
	s.mu.Lock()
	s.flushCount++
	s.mu.Unlock()
}

// MarkAttributionBoundary records the boundary-flush call. Tests that
// want to assert FlipABNode flushed before SetActivePull can read
// boundaryCalls. The implementation flushes (consistent with the real
// Mutator) so any deltas accumulated mid-test still flow through.
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
}

func (s *fakeDeltaSink) BindActiveBin(nodeID, binID int64) error {
	s.mu.Lock()
	s.bindCalls = append(s.bindCalls, fakeBindCall{nodeID, binID})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeActiveBinID(nodeID, &binID)
	}
	return nil
}

func (s *fakeDeltaSink) ClearActiveBin(nodeID int64) error {
	s.mu.Lock()
	s.clearActiveCalls = append(s.clearActiveCalls, nodeID)
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeActiveBinID(nodeID, nil)
	}
	return nil
}

type fakePrepareIncomingCall struct {
	NodeID int64
	BinID  int64
	UOP    int
}

func (s *fakeDeltaSink) PrepareIncoming(nodeID, binID int64, uop int) error {
	s.mu.Lock()
	s.prepareIncomingCalls = append(s.prepareIncomingCalls, fakePrepareIncomingCall{nodeID, binID, uop})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeCachedBin(nodeID, &binID, uop)
	}
	return nil
}

func (s *fakeDeltaSink) ClearCache(nodeID int64) error {
	s.mu.Lock()
	s.clearCacheCalls = append(s.clearCacheCalls, nodeID)
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeCachedBin(nodeID, nil, 0)
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
	UOP     int
}

func (s *fakeDeltaSink) ManualLoad(nodeID int64, activeClaimID *int64, binID *int64, uop int) error {
	s.mu.Lock()
	s.manualLoadCalls = append(s.manualLoadCalls, fakeManualLoadCall{nodeID, activeClaimID, binID, uop})
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetProcessNodeRuntimeWithBin(nodeID, activeClaimID, binID, uop)
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

type fakeAdjustBucketCall struct {
	NodeID             int64
	CoreNodeName       string
	PairKey            string
	StyleID            int64
	PartNumber         string
	CurrentQty, NewQty int
	Reason             protocol.LinesideBucketDeltaReason
}

// CaptureToLineside records the call + mirrors the real verb's
// emission shape onto bucketCalls / binCalls when db is set.
// Performs the bucket DB writes via the real *store.DB so tests
// asserting on post-state see the bucket rows update.
func (s *fakeDeltaSink) CaptureToLineside(ev uop.CaptureEvent) (int, error) {
	s.mu.Lock()
	s.captureToLinesideCalls = append(s.captureToLinesideCalls, ev)
	db := s.db
	s.mu.Unlock()
	type captureWriter interface {
		CaptureLinesideBucket(nodeID int64, pairKey string, styleID int64, partNumber string, qty int) (*lineside.Bucket, error)
		DeactivateOtherLinesideStyles(nodeID int64, styleID int64) error
	}
	var cw captureWriter
	if db != nil {
		cw, _ = db.(captureWriter)
	}
	capturedTotal := 0
	if ev.Disposition.Mode == uop.DispositionCaptureLineside {
		for part, qty := range ev.Disposition.LinesideCapture {
			if qty <= 0 || part == "" {
				continue
			}
			if cw != nil {
				if _, err := cw.CaptureLinesideBucket(ev.NodeID, ev.PairKey, ev.StyleID, part, qty); err != nil {
					return capturedTotal, err
				}
			}
			s.mu.Lock()
			s.bucketCalls = append(s.bucketCalls, fakeBucketCall{ev.NodeID, ev.PairKey, ev.StyleID, part, ev.PayloadCode, qty, protocol.ReasonCaptureFill})
			s.mu.Unlock()
			capturedTotal += qty
		}
	}
	if cw != nil {
		if err := cw.DeactivateOtherLinesideStyles(ev.NodeID, ev.StyleID); err != nil {
			return capturedTotal, err
		}
	}
	if capturedTotal > 0 && !ev.SuppressBinDelta {
		if ev.BinID > 0 {
			s.mu.Lock()
			s.binCalls = append(s.binCalls, fakeBinCall{ev.BinID, ev.PayloadCode, -capturedTotal, protocol.ReasonCaptureReduction, ev.BinEpoch})
			s.mu.Unlock()
		} else {
			// Mirror the real verb's loud diagnostic when the caller
			// couldn't resolve a bin id. The release-path falls back
			// to a legacy RemainingUOP=&0 wipe but the recurrence
			// must remain visible in operator logs.
			log.Printf("ERROR: uop capture: capture_reduction skipped (BinID=0) node=%d style=%d payload=%q captured_total=%d disposition=%q",
				ev.NodeID, ev.StyleID, ev.PayloadCode, capturedTotal, ev.Disposition.Mode)
		}
	}
	return capturedTotal, nil
}

// Consumed / Produced / Fallthrough delegate to the underlying
// accumulator-equivalent: append fakeBinCall / fakeBucketCall entries
// matching what the real verb would emit. Lets existing tests asserting
// on binCalls / bucketCalls keep working without modification.
func (s *fakeDeltaSink) Consumed(ev uop.TickEvent) error {
	s.mu.Lock()
	for part, d := range ev.Drains {
		if d.Qty > 0 {
			styleID := d.StyleID
			if styleID == 0 {
				styleID = ev.StyleID
			}
			s.bucketCalls = append(s.bucketCalls, fakeBucketCall{ev.NodeID, ev.PairKey, styleID, part, ev.PayloadCode, -d.Qty, protocol.ReasonConsumeDrain})
		}
	}
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		s.binCalls = append(s.binCalls, fakeBinCall{ev.BinID, ev.PayloadCode, -ev.BinRemainder, protocol.ReasonConsumeTick, ev.BinEpoch})
	}
	s.mu.Unlock()
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
	s.mu.Lock()
	for part, d := range ev.Drains {
		if d.Qty > 0 {
			styleID := d.StyleID
			if styleID == 0 {
				styleID = ev.StyleID
			}
			s.bucketCalls = append(s.bucketCalls, fakeBucketCall{ev.NodeID, ev.PairKey, styleID, part, ev.PayloadCode, -d.Qty, protocol.ReasonConsumeDrain})
		}
	}
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		s.binCalls = append(s.binCalls, fakeBinCall{ev.BinID, ev.PayloadCode, -ev.BinRemainder, protocol.ReasonABFallthrough, ev.BinEpoch})
	}
	s.mu.Unlock()
	return nil
}

// Backfill mirrors the real Mutator's Backfill when db is set: walk
// every node's non-empty buckets, record a bucket call per row with
// reason=capture_fill. When db is unset, returns (0, nil) — tests
// that don't care about backfill output can leave db unset.
func (s *fakeDeltaSink) Backfill(force bool) (int, error) {
	s.mu.Lock()
	s.backfillCalls++
	if force {
		s.flushCount++
	}
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return 0, nil
	}
	// Use the same db field for the read surface (writeActiveBinIDer is
	// a write-shape interface; the listing methods are on *store.DB
	// directly so we cast through the lister interface below).
	type backfillLister interface {
		ListProcessNodes() ([]processes.Node, error)
		ListLinesideBuckets(nodeID int64) ([]lineside.Bucket, error)
	}
	lister, ok := db.(backfillLister)
	if !ok {
		return 0, nil
	}
	nodes, err := lister.ListProcessNodes()
	if err != nil {
		return 0, err
	}
	emitted := 0
	for _, n := range nodes {
		buckets, err := lister.ListLinesideBuckets(n.ID)
		if err != nil {
			continue
		}
		for _, b := range buckets {
			if b.Qty <= 0 {
				continue
			}
			s.mu.Lock()
			s.bucketCalls = append(s.bucketCalls, fakeBucketCall{b.NodeID, b.PairKey, b.StyleID, b.PartNumber, "", b.Qty, protocol.ReasonCaptureFill})
			s.mu.Unlock()
			emitted++
		}
	}
	return emitted, nil
}

func (s *fakeDeltaSink) AdjustBucket(nodeID int64, coreNodeName, pairKey string, styleID int64, partNumber string, currentQty, newQty int, reason protocol.LinesideBucketDeltaReason) error {
	s.mu.Lock()
	s.adjustBucketCalls = append(s.adjustBucketCalls, fakeAdjustBucketCall{nodeID, coreNodeName, pairKey, styleID, partNumber, currentQty, newQty, reason})
	delta := newQty - currentQty
	if delta != 0 {
		s.bucketCalls = append(s.bucketCalls, fakeBucketCall{nodeID, pairKey, styleID, partNumber, "", delta, reason})
	}
	s.flushCount++
	db := s.db
	s.mu.Unlock()
	if db != nil {
		return db.SetLinesideBucketForReconcile(nodeID, pairKey, styleID, partNumber, newQty)
	}
	return nil
}

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
		&nodeID, false, 1, "DRAIN-ATTR-NODE", "", "", "", false, "PART-DRAIN")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	testutil.MustNoErr(t, db.UpdateOrderBinID(orderID, &bid), "set order bin id")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "set runtime orders")
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bid), "set active_bin_id")

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
