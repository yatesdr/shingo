package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shingoedge/store"
	"shingoedge/store/processes"
)

type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLogger) Log(format string, args ...interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, sprintfish(format, args...))
}

func (c *captureLogger) Lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

func (c *captureLogger) Contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ln := range c.lines {
		if strings.Contains(ln, substr) {
			return true
		}
	}
	return false
}

// sprintfish is a tiny formatter that keeps the test independent of
// fmt.Sprintf — easier to grep on raw arg values when the format
// string isn't directly usable.
func sprintfish(format string, args ...interface{}) string {
	var sb strings.Builder
	sb.WriteString(format)
	for _, a := range args {
		sb.WriteString(" | ")
		switch v := a.(type) {
		case string:
			sb.WriteString(v)
		default:
			sb.WriteString(stringify(v))
		}
	}
	return sb.String()
}

func stringify(v interface{}) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	switch x := v.(type) {
	case int:
		return itoa(int64(x))
	case int64:
		return itoa(x)
	}
	return ""
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// reconcilerTestEngine builds an Engine wired against a fake Core
// HTTP server and a real SQLite DB. The returned captureLogger
// records every log line so tests can assert on drift messages.
func reconcilerTestEngine(t *testing.T, db *store.DB, mockCoreURL string) (*Engine, *captureLogger) {
	t.Helper()
	eng := testEngine(t, db)
	logger := &captureLogger{}
	eng.logFn = logger.Log
	eng.coreClient = NewCoreClient(mockCoreURL)
	return eng, logger
}

// seedReconcilerNode creates a process node + active claim that
// reconciler tests can use to populate local buckets and match the
// node names sent by the fake Core. Returns nodeID, styleID, claimID.
func seedReconcilerNode(t *testing.T, db *store.DB, prefix, payloadCode string) (nodeID, styleID, claimID int64) {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", prefix+" rec", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: prefix + "-NODE",
		Code:         prefix[:3],
		Name:         prefix + " Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err = db.CreateStyle(prefix+"-STYLE", prefix+" style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)
	claimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: prefix + "-NODE",
		Role:         "consume",
		SwapMode:     "simple",
		PayloadCode:  payloadCode,
		UOPCapacity:  100,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	return nodeID, styleID, claimID
}

// TestRegression_ReconcilerLogsBucketDrift pins the bucket
// comparison side: a local bucket with a different qty than Core's
// shadow logs as drift. The local bucket Edge has but Core doesn't
// also logs (covers the in-flight delta case).
func TestRegression_ReconcilerLogsBucketDrift(t *testing.T) {
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "REC-BUCK-DRIFT", "PART-RBK")

	// Local Edge has bucket qty=5; Core's shadow says qty=2.
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-RBK", 5); err != nil {
		t.Fatalf("capture local bucket: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Buckets: []LinesideBucketRow{
				{
					NodeName: "REC-BUCK-DRIFT-NODE",
					PairKey: "", StyleID: styleID,
					PartNumber: "PART-RBK", Qty: 2,
				},
			},
		})
	}))
	defer srv.Close()

	eng, logger := reconcilerTestEngine(t, db, srv.URL)
	eng.Reconcile(true)

	if !logger.Contains("bucket drift") {
		t.Errorf("expected 'bucket drift' log line, got: %v", logger.Lines())
	}
	m := eng.ReconcilerMetrics()
	if m.BucketsDrifted == 0 {
		t.Errorf("BucketsDrifted = %d, want > 0", m.BucketsDrifted)
	}
}

// TestRegression_ReconcilerSinceLastPassGate pins the
// Decision #3 cadence: a Reconcile call within the gate window is
// silently skipped. force=true bypasses. Without this gate the
// piggybacked telemetry refresh would burn HTTP roundtrips on every
// startup-reconcile sequence.
func TestRegression_ReconcilerSinceLastPassGate(t *testing.T) {
	db := testEngineDB(t)
	_, _, _ = seedReconcilerNode(t, db, "REC-GATE", "PART-RG")

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(UOPStateResponse{})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	eng.SetReconcileInterval(1 * time.Hour)

	eng.Reconcile(false) // first pass — runs
	eng.Reconcile(false) // gated — no run
	eng.Reconcile(false) // gated — no run

	if calls != 1 {
		t.Errorf("HTTP calls = %d, want 1 (since-last-pass gate must skip subsequent passes)", calls)
	}

	eng.Reconcile(true) // force — runs regardless of gate
	if calls != 2 {
		t.Errorf("HTTP calls = %d, want 2 after force=true", calls)
	}
}

// TestRegression_ReconciliationSelfHeal pins the unconditional self-
// heal contract: a drifted local runtime is overwritten from Core's
// authoritative bin count. The reconciler has no off-switch — it is
// the mechanism that keeps Edge's cache in lockstep with Core.
func TestRegression_ReconciliationSelfHeal(t *testing.T) {
	db := testEngineDB(t)
	nodeID, _, claimID := seedReconcilerNode(t, db, "REC-SELF-HEAL", "PART-RSH")

	// Local runtime says 12. Use claimID (not styleID) — they're
	// distinct identifiers and runtime expects the claim's row id.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 12); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Core's authoritative bin says 47 (the bin that's currently at
	// the slot has 47 UOP left). The local runtime should heal to 47.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Bins: []BinUOPRow{
				{
					BinID: 555, NodeName: "REC-SELF-HEAL-NODE",
					PayloadCode:  "PART-RSH",
					UOPRemaining: 47,
				},
			},
		})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)

	eng.Reconcile(true)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 47 {
		t.Errorf("runtime.RemainingUOPCached = %d, want 47 (self-heal must overwrite local from Core)", rt.RemainingUOPCached)
	}

	m := eng.ReconcilerMetrics()
	if m.BinsHealed != 1 {
		t.Errorf("BinsHealed = %d, want 1", m.BinsHealed)
	}
}

// TestReconciler_SkipsHealWhenDeltaPending pins the Item 2 pending-
// delta guard: when the inventory delta reporter has unflushed (or
// unsent) activity for a bin, the reconciler must skip healing that
// bin's runtime cache. Without the guard, Core's snapshot is stale
// relative to Edge's pipeline (delta in flight), and overwriting Edge
// with Core's value would erase the in-flight count change.
func TestReconciler_SkipsHealWhenDeltaPending(t *testing.T) {
	db := testEngineDB(t)
	nodeID, _, claimID := seedReconcilerNode(t, db, "REC-PEND-SKIP", "PART-RPS")

	// Local runtime is 12 (post-tick); Core's snapshot still says 47
	// (pre-tick). With pending guard, runtime stays 12. Without the
	// guard, runtime would heal to 47 and lose the in-flight tick.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 12); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	const binID int64 = 777
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Bins: []BinUOPRow{
				{BinID: binID, NodeName: "REC-PEND-SKIP-NODE", UOPRemaining: 47},
			},
		})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	sink := &fakeDeltaSink{
		pendingBins: map[int64]struct{}{binID: {}},
	}
	eng.SetInventoryDeltaSink(sink)

	eng.Reconcile(true)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 12 {
		t.Errorf("runtime.RemainingUOPCached = %d, want 12 (must skip heal when delta is pending — would stomp in-flight tick)",
			rt.RemainingUOPCached)
	}
	m := eng.ReconcilerMetrics()
	if m.BinsHealed != 0 {
		t.Errorf("BinsHealed = %d, want 0 (skipped, not healed)", m.BinsHealed)
	}
	if m.BinsSkipped != 1 {
		t.Errorf("BinsSkipped = %d, want 1 (the pending bin was skipped)", m.BinsSkipped)
	}
}

// TestReconciler_HealsAfterFlushClears is the positive companion: once
// the reporter clears its pending entry (flush succeeded, delta is at
// Core), the next reconciliation pass heals as normal. Pin: the guard
// is a *skip-this-pass*, not a permanent block.
func TestReconciler_HealsAfterFlushClears(t *testing.T) {
	db := testEngineDB(t)
	nodeID, _, claimID := seedReconcilerNode(t, db, "REC-PEND-CLEAR", "PART-RPC")

	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 12); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	const binID int64 = 778
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Bins: []BinUOPRow{
				{BinID: binID, NodeName: "REC-PEND-CLEAR-NODE", UOPRemaining: 47},
			},
		})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	sink := &fakeDeltaSink{
		pendingBins: map[int64]struct{}{binID: {}},
	}
	eng.SetInventoryDeltaSink(sink)

	// Pass 1: pending → skipped, runtime stays at 12.
	eng.Reconcile(true)
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 12 {
		t.Fatalf("pass-1 runtime = %d, want 12 (pending skip)", rt.RemainingUOPCached)
	}

	// Reporter "flushes" — clear the pending entry the same way the
	// real reporter would after a successful EnqueueOutbox.
	sink.mu.Lock()
	delete(sink.pendingBins, binID)
	sink.mu.Unlock()

	// Pass 2: no longer pending → heals, runtime moves to 47.
	eng.Reconcile(true)
	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 47 {
		t.Errorf("pass-2 runtime = %d, want 47 (heal proceeds once pending cleared)",
			rt.RemainingUOPCached)
	}
	m := eng.ReconcilerMetrics()
	if m.BinsHealed != 1 {
		t.Errorf("BinsHealed = %d, want 1", m.BinsHealed)
	}
	if m.BinsSkipped != 1 {
		t.Errorf("BinsSkipped = %d, want 1 (pass-1 skip)", m.BinsSkipped)
	}
}

// TestRegression_ReconcilerCoreUnreachableNoCrash pins graceful
// degradation: when Core's HTTP server is down, the reconciler logs
// a marker and returns without panic or partial-state mutation.
// Phase 2 is observation-only; a missed pass is acceptable.
func TestRegression_ReconcilerCoreUnreachableNoCrash(t *testing.T) {
	db := testEngineDB(t)
	_, _, _ = seedReconcilerNode(t, db, "REC-UNREACH", "PART-RU")

	// Point at a closed port — Get will fail.
	eng, logger := reconcilerTestEngine(t, db, "http://127.0.0.1:1") // port 1 is reserved/closed

	eng.Reconcile(true)

	if !logger.Contains("uop_reconciler") {
		t.Errorf("expected reconciler log marker, got: %v", logger.Lines())
	}
	// Must have completed a pass (counted) without writing any
	// drift records (the snapshot was empty/unreachable).
	m := eng.ReconcilerMetrics()
	if m.Passes != 1 {
		t.Errorf("Passes = %d, want 1 (Reconcile must complete even when Core unreachable)", m.Passes)
	}
}

// TestReconciler_EmptySlotDetection_ZerosRuntime pins Item 4: when
// Core's snapshot omits a node Edge tracks (the slot has no bin),
// Edge's runtime cache must zero out. Without this pass, a station
// whose bin walked off without a normal completion event sits showing
// stale UOP forever — operators see "fresh" inventory at an empty
// slot until manual intervention.
func TestReconciler_EmptySlotDetection_ZerosRuntime(t *testing.T) {
	db := testEngineDB(t)
	nodeID, _, claimID := seedReconcilerNode(t, db, "REC-EMPTY", "PART-RE")

	// Local runtime says 50 (stale — bin physically walked off).
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Core's snapshot returns NO bins — confirms no bin at this slot.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{Bins: nil})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)

	eng.Reconcile(true)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 0 {
		t.Errorf("runtime.RemainingUOPCached = %d, want 0 (Core reports no bin → empty slot heal must zero)", rt.RemainingUOPCached)
	}
	if rt.ActiveClaimID != nil {
		t.Errorf("ActiveClaimID = %v, want nil (empty slot must clear claim anchor)", rt.ActiveClaimID)
	}
	if eng.ReconcilerMetrics().BinsHealed != 1 {
		t.Errorf("BinsHealed = %d, want 1 (heal-empty counts toward heal total)",
			eng.ReconcilerMetrics().BinsHealed)
	}
}

// TestReconciler_BucketSelfHeal_LocalLessThanCore pins Item 5: when
// Edge's local bucket qty is below Core's authoritative shadow, the
// reconciler heals up to Core's value. Pre-Item-5 the drift counter
// just incremented; nothing wrote.
func TestReconciler_BucketSelfHeal_LocalLessThanCore(t *testing.T) {
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "REC-BUCKET-LT", "PART-RBLT")

	// Local has 5; Core has 12.
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-RBLT", 5); err != nil {
		t.Fatalf("capture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Buckets: []LinesideBucketRow{
				{NodeName: "REC-BUCKET-LT-NODE", PartNumber: "PART-RBLT",
					StyleID: styleID, PairKey: "", Qty: 12},
			},
		})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	eng.Reconcile(true)

	got, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-RBLT")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if got.Qty != 12 {
		t.Errorf("qty = %d, want 12 (heal up to Core)", got.Qty)
	}
	if eng.ReconcilerMetrics().BucketsHealed != 1 {
		t.Errorf("BucketsHealed = %d, want 1", eng.ReconcilerMetrics().BucketsHealed)
	}
}

// TestReconciler_BucketSelfHeal_LocalGreaterThanCore pins the inverse
// case: local has more than Core, heal down to Core's value.
func TestReconciler_BucketSelfHeal_LocalGreaterThanCore(t *testing.T) {
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "REC-BUCKET-GT", "PART-RBGT")

	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-RBGT", 20); err != nil {
		t.Fatalf("capture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Buckets: []LinesideBucketRow{
				{NodeName: "REC-BUCKET-GT-NODE", PartNumber: "PART-RBGT",
					StyleID: styleID, PairKey: "", Qty: 7},
			},
		})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	eng.Reconcile(true)

	got, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-RBGT")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if got.Qty != 7 {
		t.Errorf("qty = %d, want 7 (heal down to Core)", got.Qty)
	}
	if eng.ReconcilerMetrics().BucketsHealed != 1 {
		t.Errorf("BucketsHealed = %d, want 1", eng.ReconcilerMetrics().BucketsHealed)
	}
}

// TestReconciler_BucketSelfHeal_LocalMissing pins the local-only
// inverse: Edge has a bucket Core doesn't know about. Core wins —
// delete the local row.
func TestReconciler_BucketSelfHeal_LocalMissing(t *testing.T) {
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "REC-BUCKET-LOC", "PART-RBLOC")

	// Edge has 9 of PART-RBLOC; Core's snapshot is empty.
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-RBLOC", 9); err != nil {
		t.Fatalf("capture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{Buckets: nil})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	eng.Reconcile(true)

	got, _ := db.FindLinesideBucket(nodeID, styleID, "PART-RBLOC")
	if got != nil {
		t.Errorf("local bucket still present qty=%d, want deleted (heal toward empty Core)", got.Qty)
	}
	if eng.ReconcilerMetrics().BucketsHealed != 1 {
		t.Errorf("BucketsHealed = %d, want 1", eng.ReconcilerMetrics().BucketsHealed)
	}
}

// TestReconciler_BucketSelfHeal_RespectsPendingDeltaGuard pins the
// pending-delta gate on the bucket path. While the reporter has
// unflushed activity for a (node, style, part) scope, the heal must
// skip — symmetric to the bin-side guard from Item 2.
func TestReconciler_BucketSelfHeal_RespectsPendingDeltaGuard(t *testing.T) {
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "REC-BUCKET-PEND", "PART-RBP")

	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-RBP", 5); err != nil {
		t.Fatalf("capture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Buckets: []LinesideBucketRow{
				{NodeName: "REC-BUCKET-PEND-NODE", PartNumber: "PART-RBP",
					StyleID: styleID, PairKey: "", Qty: 99},
			},
		})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	sink := &fakeDeltaSink{
		pendingBuckets: map[fakePendingBucketKey]struct{}{
			{nodeID: nodeID, styleID: styleID, partNumber: "PART-RBP"}: {},
		},
	}
	eng.SetInventoryDeltaSink(sink)

	eng.Reconcile(true)

	got, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-RBP")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if got.Qty != 5 {
		t.Errorf("qty = %d, want 5 (heal must skip when delta pending — would stomp in-flight delta)",
			got.Qty)
	}
	if eng.ReconcilerMetrics().BucketsHealed != 0 {
		t.Errorf("BucketsHealed = %d, want 0 (skipped, not healed)",
			eng.ReconcilerMetrics().BucketsHealed)
	}
	if eng.ReconcilerMetrics().BucketsSkipped != 1 {
		t.Errorf("BucketsSkipped = %d, want 1", eng.ReconcilerMetrics().BucketsSkipped)
	}
}

// TestRegression_ABFlipAtomic_NoTickMisattribution pins the Item 5
// transactional wrap: the two SetActivePull writes inside FlipABNode
// land in a single SQLite transaction, so a tick firing between them
// can never observe both sides inactive (or both active) and
// mis-attribute. We don't try to race this in the test (SQLite's
// max-open-conns=1 setting and the writer-lock model already
// serialize); instead we pin the pair-state invariant after FlipABNode:
// the two nodes have flipped, and they are NOT in any transient
// inconsistent state (both true, both false).
func TestRegression_ABFlipAtomic_NoTickMisattribution(t *testing.T) {
	db := testEngineDB(t)
	_, nodeAID, nodeBID, _, _, _ := seedABPair(t, db)

	// Pre-flip: A=active, B=inactive (per seedABPair).
	rtA, _ := db.GetProcessNodeRuntime(nodeAID)
	rtB, _ := db.GetProcessNodeRuntime(nodeBID)
	if !rtA.ActivePull || rtB.ActivePull {
		t.Fatalf("pre-flip A.active=%t B.active=%t, want true/false",
			rtA.ActivePull, rtB.ActivePull)
	}

	eng := testEngine(t, db)

	// Flip B → active. The tx wrap is invisible at the API level; the
	// caller can only verify that BOTH writes happened.
	if err := eng.FlipABNode(nodeBID); err != nil {
		t.Fatalf("FlipABNode: %v", err)
	}

	rtA, _ = db.GetProcessNodeRuntime(nodeAID)
	rtB, _ = db.GetProcessNodeRuntime(nodeBID)
	if rtA.ActivePull || !rtB.ActivePull {
		t.Errorf("post-flip A.active=%t B.active=%t, want false/true (atomic flip must complete both writes)",
			rtA.ActivePull, rtB.ActivePull)
	}

	// Flip back → A active.
	if err := eng.FlipABNode(nodeAID); err != nil {
		t.Fatalf("FlipABNode back: %v", err)
	}
	rtA, _ = db.GetProcessNodeRuntime(nodeAID)
	rtB, _ = db.GetProcessNodeRuntime(nodeBID)
	if !rtA.ActivePull || rtB.ActivePull {
		t.Errorf("post-second-flip A.active=%t B.active=%t, want true/false",
			rtA.ActivePull, rtB.ActivePull)
	}
}

// TestReconciler_EmptySlot_NoOpIfAlreadyZero pins the no-op invariant:
// when the slot is already zero, the heal-empty path must NOT write.
// Without this guard, every reconcile pass against an empty slot
// would issue a redundant SetProcessNodeRuntime call — wasted IO and
// noisy audit trail. The check is `runtime.RemainingUOPCached == 0 →
// continue` before the heal call.
func TestReconciler_EmptySlot_NoOpIfAlreadyZero(t *testing.T) {
	db := testEngineDB(t)
	nodeID, _, claimID := seedReconcilerNode(t, db, "REC-EMPTY-NOOP", "PART-REN")

	// Runtime is already zero (slot was previously emptied by another
	// pass, or boot-time default).
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 0); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{Bins: nil})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)

	eng.Reconcile(true)

	if eng.ReconcilerMetrics().BinsHealed != 0 {
		t.Errorf("BinsHealed = %d, want 0 (already-zero slot must not count as healed)",
			eng.ReconcilerMetrics().BinsHealed)
	}

	// Sanity: runtime stays at zero (the no-op didn't accidentally
	// write some other value).
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 0 {
		t.Errorf("runtime.RemainingUOPCached = %d, want 0", rt.RemainingUOPCached)
	}
}
