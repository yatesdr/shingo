package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
)

// TestPhase3Backfill_InFlightBuckets pins Phase 3f's backfill
// contract: every local active node_lineside_bucket row produces one
// LinesideBucketDelta(capture_fill, +qty) on the reporter's
// accumulator. The seed-formula matches plan §Phase 3 backfill
// section: lineside_bucket.qty_at_backfill = current local qty.
//
// This is the in-flight bucket case Dev 4 called out: a bucket that
// was partially drained (started at 47, drained 12 → local qty 35)
// emits a +35 seed delta, NOT +47. Core's dedup gives at-least-once
// delivery; idempotent because the seed is always "the qty as
// observed right now."
func TestPhase3Backfill_InFlightBuckets(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "BACKFILL", "PART-BF")

	// Seed two buckets at different states. The 35-qty bucket is the
	// in-flight case (partially drained); the 50-qty is brand new.
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-BF", 35); err != nil {
		t.Fatalf("seed bucket 1: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeID, "L1|U1", styleID, "PART-BF-2", 50); err != nil {
		t.Fatalf("seed bucket 2: %v", err)
	}

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	emitted, err := eng.BackfillBucketsForStation(false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if emitted != 2 {
		t.Errorf("emitted = %d, want 2 (one delta per non-empty bucket)", emitted)
	}

	if len(sink.bucketCalls) != 2 {
		t.Fatalf("bucket calls = %d, want 2: %+v", len(sink.bucketCalls), sink.bucketCalls)
	}
	byPart := map[string]fakeBucketCall{}
	for _, c := range sink.bucketCalls {
		byPart[c.PartNumber] = c
	}
	if byPart["PART-BF"].Delta != 35 {
		t.Errorf("PART-BF seed delta = %d, want 35 (in-flight qty)", byPart["PART-BF"].Delta)
	}
	if byPart["PART-BF"].Reason != protocol.ReasonCaptureFill {
		t.Errorf("reason = %q, want capture_fill", byPart["PART-BF"].Reason)
	}
	if byPart["PART-BF-2"].Delta != 50 {
		t.Errorf("PART-BF-2 seed delta = %d, want 50", byPart["PART-BF-2"].Delta)
	}
}

// TestPhase3Backfill_EmptyBucketsNotEmitted pins the no-op contract:
// a row with qty=0 (e.g. a bucket that drained to zero just before
// backfill ran) doesn't generate a seed delta. Avoids burning
// SequenceID slots and the dedup row count for a no-op.
func TestPhase3Backfill_EmptyBucketsNotEmitted(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "BACKFILL-EMPTY", "PART-BFE")

	// Capture then fully drain so the row exists with qty=0.
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-BFE", 5); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.DrainLinesideBucket(nodeID, styleID, "PART-BFE", 5); err != nil {
		t.Fatalf("drain: %v", err)
	}

	eng := testEngine(t, db)
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	emitted, err := eng.BackfillBucketsForStation(false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if emitted != 0 {
		t.Errorf("emitted = %d, want 0 (zero-qty rows must not emit)", emitted)
	}
	if len(sink.bucketCalls) != 0 {
		t.Errorf("bucket calls = %d, want 0", len(sink.bucketCalls))
	}
}

// TestBucketBackfillNeeded_FreshCoreEdgeHasRows pins the Item 3 auto-
// fire trigger: when Core reports zero buckets for the station and
// Edge has at least one local bucket row, the helper returns true.
// The startup auto-fire path keys on this; without it Core stays
// empty after deployment until an operator manually backfills.
func TestBucketBackfillNeeded_FreshCoreEdgeHasRows(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "BF-NEEDED", "PART-BFN")
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-BFN", 8); err != nil {
		t.Fatalf("capture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{Buckets: nil})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	needed, err := eng.BucketBackfillNeeded()
	if err != nil {
		t.Fatalf("BucketBackfillNeeded: %v", err)
	}
	if !needed {
		t.Errorf("needed = false, want true (fresh Core + Edge has rows)")
	}
}

// TestBucketBackfillNeeded_PopulatedCoreReturnsFalse pins the idempotent
// gate: when Core already has buckets for this station, the helper
// returns false even if Edge also has rows. Re-runs of Edge against a
// populated Core must not re-seed (would double-count via dedup
// at-least-once semantics).
func TestBucketBackfillNeeded_PopulatedCoreReturnsFalse(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, styleID, _ := seedReconcilerNode(t, db, "BF-POP", "PART-BFP")
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-BFP", 8); err != nil {
		t.Fatalf("capture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{
			Buckets: []LinesideBucketRow{
				{NodeName: "BF-POP-NODE", PartNumber: "PART-BFP",
					StyleID: styleID, Qty: 8},
			},
		})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	needed, err := eng.BucketBackfillNeeded()
	if err != nil {
		t.Fatalf("BucketBackfillNeeded: %v", err)
	}
	if needed {
		t.Errorf("needed = true, want false (Core already populated)")
	}
}

// TestBucketBackfillNeeded_EmptyEdgeReturnsFalse pins the no-data case:
// Edge has nothing to seed (truly fresh deployment), so the helper
// returns false even though Core's bucket list is empty. Avoids a
// no-op backfill emission.
func TestBucketBackfillNeeded_EmptyEdgeReturnsFalse(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, _, _ = seedReconcilerNode(t, db, "BF-EMPTY", "PART-BFE2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UOPStateResponse{Buckets: nil})
	}))
	defer srv.Close()

	eng, _ := reconcilerTestEngine(t, db, srv.URL)
	needed, err := eng.BucketBackfillNeeded()
	if err != nil {
		t.Fatalf("BucketBackfillNeeded: %v", err)
	}
	if needed {
		t.Errorf("needed = true, want false (Edge has no rows)")
	}
}

// TestPhase3Backfill_NoSinkErrors pins the precondition: backfill
// without a reporter wired returns an error rather than silently
// emitting into the void. Catches a misconfigured composition root.
func TestPhase3Backfill_NoSinkErrors(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	// Deliberately clear the default sink testEngine wires — this test
	// pins the precondition that backfill without a reporter wired
	// returns an error rather than silently emitting into the void.
	eng.SetInventoryDeltaSink(nil)

	if _, err := eng.BackfillBucketsForStation(false); err == nil {
		t.Error("err = nil, want non-nil when reporter unset")
	}
}
