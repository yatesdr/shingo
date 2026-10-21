package engine

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestReleaseCaptureLineside_NilBinID_ResolvesFromCore pins the
// nil-order.BinID resolve path (Issue 1, lineside-buckets-investigation-
// 2026-05-18.md). When the operator picks PULL PARTS LINESIDE on a
// release whose order.BinID is nil (REP / complex orders whose
// OrderDelivered reply didn't carry binID), Edge must resolve the bin
// currently sitting at the slot from the bins table (site 14 of the reads)
// and thread that resolved id into the CaptureEvent so the paired
// BinUOPDelta(capture_reduction) lands on the right bin row.
//
// Without this resolve the delta is dropped at the BinID==0 gate in
// capture.go and the bin returns to the marshalling area with its
// original UOP intact — the plant-visible symptom Ryan reported.
func TestReleaseCaptureLineside_NilBinID_ResolvesFromCore(t *testing.T) {
	t.Parallel()
	const resolvedBinID int64 = 7777

	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REL-NIL-RESOLVE", PayloadCode: "PART-NIL-RES", UOPCapacity: 200, InitialUOP: 200,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 200), "seed runtime")

	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-nil-resolve")
	// order.BinID intentionally left nil — this is the broken state from
	// the plant incident.
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	// No HTTP read: the bin at the slot is the table's.
	coreNeverAnswers(t, eng)
	holdActableBins(t, eng, protocol.NodeBinsRow{Node: "REL-NIL-RESOLVE-NODE", BinID: resolvedBinID,
		BinLabel: "RESOLVED-BIN", PayloadCode: "PART-NIL-RES", Stocked: true, Partial: true, UOPCapacity: 200, Occupied: true})
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-NIL-RES": 96},
		CalledBy:        "test-op",
	}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "ReleaseOrderWithLineside")

	// Wire RemainingUOP must stay nil — the delta stream owns the count
	// change now that we resolved the bin.
	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes = %d, want 1", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])
	if rel.RemainingUOP != nil {
		t.Errorf("wire RemainingUOP = %d, want nil — resolved BinID > 0 keeps the delta-only path",
			*rel.RemainingUOP)
	}

	// Exactly one BinUOPDelta(capture_reduction) against the resolved id.
	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	bc := sink.binCalls[0]
	if bc.BinID != resolvedBinID {
		t.Errorf("bin call BinID = %d, want %d (resolved from the bins table)", bc.BinID, resolvedBinID)
	}
	if bc.Delta != -96 {
		t.Errorf("bin call delta = %d, want -96", bc.Delta)
	}
	if bc.Reason != protocol.ReasonCaptureReduction {
		t.Errorf("bin call reason = %q, want %q", bc.Reason, protocol.ReasonCaptureReduction)
	}
}

// TestReleaseCaptureLineside_NilBinID_Unresolvable_FallsBackToZero
// pins the safety-net path: when order.BinID is nil AND the bins table
// can't name a bin (an empty slot, or a table that may not be decided
// from), the wire shape reverts to the legacy RemainingUOP=&0 so Core's
// SyncOrClearForReleased(0) wipes the manifest the old way. Without the
// fallback the bin row would silently keep its original count — the exact
// silent-drop bug.
func TestReleaseCaptureLineside_NilBinID_Unresolvable_FallsBackToZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, eng *Engine)
	}{
		// The table holds an unoccupied slot (no bin to resolve).
		{"empty slot", func(t *testing.T, eng *Engine) {
			holdActableBins(t, eng, protocol.NodeBinsRow{Node: "REL-NIL-FB-NODE"})
		}},
		// No table held: not actable, the same fallback.
		{"not actable", func(*testing.T, *Engine) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			releaseUnresolvableFallsBackToZero(t, tc.hold)
		})
	}
}

func releaseUnresolvableFallsBackToZero(t *testing.T, hold func(*testing.T, *Engine)) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REL-NIL-FB", PayloadCode: "PART-FB", UOPCapacity: 100, InitialUOP: 100,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 100), "seed runtime")
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-nil-fb")
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	coreNeverAnswers(t, eng)
	hold(t, eng)

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-FB": 25},
		CalledBy:        "test-op",
	}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "ReleaseOrderWithLineside")

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes = %d, want 1", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])
	if rel.RemainingUOP == nil {
		t.Fatalf("wire RemainingUOP = nil, want &0 — fallback must fire when the table names no bin")
	}
	if *rel.RemainingUOP != 0 {
		t.Errorf("wire RemainingUOP = %d, want 0", *rel.RemainingUOP)
	}
}

// TestReleaseCaptureLineside_NilBinID_EmitsErrorLog pins the silent-
// drop diagnostic: when the capture path lands with capturedTotal > 0
// but BinID == 0, capture.go emits a loud error-level log. The bug
// went unnoticed in production because the failure was silent; this
// test guarantees a future regression is visible in operator logs.
func TestReleaseCaptureLineside_NilBinID_EmitsErrorLog(t *testing.T) {
	// No t.Parallel — stdlib log.SetOutput is global and parallel
	// tests would race with each other for the captured writer.
	//
	// The table holds an empty slot so BinID stays 0 through to capture.go.
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REL-NIL-LOG", PayloadCode: "PART-LOG", UOPCapacity: 50, InitialUOP: 50,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 50), "seed runtime")
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-nil-log")
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	coreNeverAnswers(t, eng)
	holdActableBins(t, eng, protocol.NodeBinsRow{Node: "REL-NIL-LOG-NODE"})

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	var buf bytes.Buffer
	prevW := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevW)
		log.SetFlags(prevFlags)
	})

	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-LOG": 17},
		CalledBy:        "test-op",
	}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "ReleaseOrderWithLineside")

	logged := buf.String()
	if !strings.Contains(logged, "ERROR") {
		t.Errorf("expected ERROR-level log line on BinID==0 silent-drop gate; got %q", logged)
	}
	if !strings.Contains(logged, "capture_reduction") {
		t.Errorf("expected log to mention capture_reduction; got %q", logged)
	}
	if !strings.Contains(logged, "captured_total=17") {
		t.Errorf("expected log to mention the captured total; got %q", logged)
	}
	if !strings.Contains(logged, "BinID=0") {
		t.Errorf("expected log to mention BinID=0 (the silent-drop gate); got %q", logged)
	}
}
