//go:build docker

package uop_test

import (
	"errors"
	"testing"
	"time"

	"shingo/protocol"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/audit"
	"shingocore/uop"
)

func makeBinDelta(binID int64, payloadCode string, delta int, seq int64, reason protocol.BinUOPDeltaReason) *protocol.BinUOPDelta {
	now := time.Now().UTC()
	return &protocol.BinUOPDelta{
		Station:     "ALN_001",
		BinID:       binID,
		PayloadCode: payloadCode,
		Delta:       delta,
		Reason:      reason,
		SequenceID:  seq,
		WindowStart: now.Add(-5 * time.Second),
		WindowEnd:   now,
	}
}

func makeBucketDelta(coreNodeName, pairKey string, styleID int64, partNumber string, delta int, seq int64, reason protocol.LinesideBucketDeltaReason) *protocol.LinesideBucketDelta {
	now := time.Now().UTC()
	return &protocol.LinesideBucketDelta{
		Station:      "ALN_001",
		CoreNodeName: coreNodeName,
		PairKey:      pairKey,
		StyleID:      styleID,
		PartNumber:   partNumber,
		Delta:        delta,
		Reason:       reason,
		SequenceID:   seq,
		WindowStart:  now.Add(-5 * time.Second),
		WindowEnd:    now,
	}
}

// TestInventoryDelta_BinUOPDelta_AppliesToAuthoritative pins the
// authoritative-write invariant: BinUOPDelta moves bins.uop_remaining
// directly — deltas land on the count the rest of the system reads.
func TestInventoryDelta_BinUOPDelta_AppliesToAuthoritative(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-1", "PART-A", 100)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-A", -3, 1, protocol.ReasonConsumeTick)), "apply consume_tick")
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-A", -2, 2, protocol.ReasonConsumeTick)), "apply consume_tick #2")

	var got int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got), "read bin")
	if got != 95 {
		t.Errorf("uop_remaining = %d, want 95 (100 - 3 - 2)", got)
	}
}

// TestInventoryDelta_BinUOPDelta_DedupesReplay pins the at-most-once
// contract: replaying the same SequenceID is silently skipped, and
// bins.uop_remaining does not accumulate the duplicate. Edge's outbox
// can replay any envelope after a Core blip; the dedup table is what
// makes that safe.
func TestInventoryDelta_BinUOPDelta_DedupesReplay(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-DUP", "PART-A", 100)

	d := makeBinDelta(bin.ID, "PART-A", -10, 5, protocol.ReasonConsumeTick)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(d), "apply first time")
	// Replay the exact same envelope.
	if err := svc.ApplyBinUOPDelta(d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("replay error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}
	// And a re-replay just to be sure the second skip didn't advance state.
	if err := svc.ApplyBinUOPDelta(d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("third replay error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}

	var got int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got), "read bin")
	if got != 90 {
		t.Errorf("uop_remaining = %d, want 90 (100 - 10 once, not 100 - 30)", got)
	}
}

// TestInventoryDelta_BinUOPDelta_OutOfOrderRejectsLowerSeq pins the
// monotonic-seq guarantee: a delta with SequenceID lower than the
// already-applied last_seq is treated as a replay and skipped. Edge
// guarantees in-order delivery for a given scope; out-of-order arrival
// indicates either a replay or a bug, and either way silently dropping
// is the safe choice.
func TestInventoryDelta_BinUOPDelta_OutOfOrderRejectsLowerSeq(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-ORD", "PART-A", 100)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-A", -5, 10, protocol.ReasonConsumeTick)), "apply seq=10")
	if err := svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-A", -7, 5, protocol.ReasonConsumeTick)); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("seq=5 (older) error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}

	var got int
	_ = db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got)
	if got != 95 {
		t.Errorf("uop_remaining = %d, want 95 (older seq must not apply; 100 - 5 only)", got)
	}
}

// TestInventoryDelta_BinUOPDelta_RejectsMismatchedPayload pins the
// validation guard: if the bin's payload was reassigned underneath the
// delta, applying it would corrupt the count attribution. Reject so a
// reconciliation pass surfaces the mismatch instead of letting it slip
// through silently.
func TestInventoryDelta_BinUOPDelta_RejectsMismatchedPayload(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-MIS", "PART-A", 100)

	err := svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-DIFFERENT", -1, 1, protocol.ReasonConsumeTick))
	if err == nil {
		t.Fatal("expected payload-mismatch error, got nil")
	}

	var got int
	_ = db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got)
	if got != 100 {
		t.Errorf("uop_remaining = %d, want 100 (mismatched delta must not apply)", got)
	}
}

// TestInventoryDelta_BinUOPDelta_RejectsUnknownBin pins the missing-
// target guard: a delta against a bin that doesn't exist is dropped
// loudly. Phase 2's reconciler picks up the divergence on the next
// pass.
func TestInventoryDelta_BinUOPDelta_RejectsUnknownBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_ = testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	if err := svc.ApplyBinUOPDelta(makeBinDelta(999999999, "PART-A", -1, 1, protocol.ReasonConsumeTick)); err == nil {
		t.Fatal("expected unknown-bin error, got nil")
	}
}

// TestInventoryDelta_LinesideBucketDelta_UpsertsAndDeletesAtZero pins
// the bucket lifecycle: capture_fill creates the row; consume_drain
// reduces it; reaching zero deletes the row. Option C (location-only)
// means an empty bucket has nothing to track.
func TestInventoryDelta_LinesideBucketDelta_UpsertsAndDeletesAtZero(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	// Round-3 Obs 8: applier validates core_node_name resolves to a
	// Core node row, so the fixture's storage node is what the delta
	// must reference. SetupStandardData creates STORAGE-A1.
	nodeName := sd.StorageNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(makeBucketDelta(nodeName, "L1|U1", 100, "PART-A", 47, 1, protocol.ReasonCaptureFill)), "capture_fill")

	var qty int
	if err := db.QueryRow(`SELECT qty FROM lineside_buckets
		WHERE station='ALN_001' AND core_node_name=$1 AND pair_key='L1|U1' AND style_id=100 AND part_number='PART-A'`, nodeName).
		Scan(&qty); err != nil {
		t.Fatalf("read bucket after fill: %v", err)
	}
	if qty != 47 {
		t.Errorf("bucket qty after fill = %d, want 47", qty)
	}

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(makeBucketDelta(nodeName, "L1|U1", 100, "PART-A", -47, 2, protocol.ReasonConsumeDrain)), "consume_drain to zero")

	var rowCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lineside_buckets
		WHERE station='ALN_001' AND core_node_name=$1 AND pair_key='L1|U1' AND style_id=100 AND part_number='PART-A'`, nodeName).
		Scan(&rowCount)
	if rowCount != 0 {
		t.Errorf("bucket row count at zero = %d, want 0 (Option C — empty buckets are deleted)", rowCount)
	}
}

// TestInventoryDelta_LinesideBucketDelta_RejectsUnderflow pins the
// CHECK (qty >= 0) constraint. A delta that would drive a bucket
// negative is rejected. Reconciliation in Phase 2 surfaces the
// divergence.
func TestInventoryDelta_LinesideBucketDelta_RejectsUnderflow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))
	nodeName := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(makeBucketDelta(nodeName, "L2|U2", 200, "PART-B", 5, 1, protocol.ReasonCaptureFill)), "capture_fill")
	// Try to drain 10 from a bucket that holds 5.
	if err := svc.ApplyLinesideBucketDelta(makeBucketDelta(nodeName, "L2|U2", 200, "PART-B", -10, 2, protocol.ReasonConsumeDrain)); err == nil {
		t.Fatal("expected CHECK violation on underflow, got nil")
	}

	// Bucket should still hold 5 — the rejected delta must not have applied.
	var qty int
	_ = db.QueryRow(`SELECT qty FROM lineside_buckets
		WHERE station='ALN_001' AND core_node_name=$1 AND pair_key='L2|U2' AND style_id=200 AND part_number='PART-B'`, nodeName).
		Scan(&qty)
	if qty != 5 {
		t.Errorf("bucket qty after rejected drain = %d, want 5", qty)
	}
}

// TestInventoryDelta_LinesideBucketDelta_DedupesReplay pins the
// at-most-once contract for the bucket scope.
func TestInventoryDelta_LinesideBucketDelta_DedupesReplay(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))
	nodeName := sd.StorageNode.Name

	d := makeBucketDelta(nodeName, "L1|U1", 300, "PART-C", 10, 1, protocol.ReasonCaptureFill)
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(d), "first apply")
	if err := svc.ApplyLinesideBucketDelta(d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("replay error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}

	var qty int
	_ = db.QueryRow(`SELECT qty FROM lineside_buckets
		WHERE station='ALN_001' AND core_node_name=$1 AND pair_key='L1|U1' AND style_id=300 AND part_number='PART-C'`, nodeName).
		Scan(&qty)
	if qty != 10 {
		t.Errorf("bucket qty after replay = %d, want 10 (delta applied once)", qty)
	}
}

// TestInventoryDelta_BucketScopeKeysIndependent pins that two buckets
// at the same node but different (pair_key, style_id, part_number)
// dedup independently. A reused SequenceID across distinct scopes is
// not a replay.
func TestInventoryDelta_BucketScopeKeysIndependent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))
	nodeName := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(makeBucketDelta(nodeName, "L1|U1", 400, "PART-D", 5, 1, protocol.ReasonCaptureFill)), "part D apply")
	// Same SequenceID, different part — this is a separate scope.
	if err := svc.ApplyLinesideBucketDelta(makeBucketDelta(nodeName, "L1|U1", 400, "PART-E", 7, 1, protocol.ReasonCaptureFill)); err != nil {
		t.Errorf("part E apply (same seq, different scope): %v", err)
	}

	var d, e int
	_ = db.QueryRow(`SELECT qty FROM lineside_buckets
		WHERE core_node_name=$1 AND part_number='PART-D'`, nodeName).Scan(&d)
	_ = db.QueryRow(`SELECT qty FROM lineside_buckets
		WHERE core_node_name=$1 AND part_number='PART-E'`, nodeName).Scan(&e)
	if d != 5 {
		t.Errorf("PART-D qty = %d, want 5", d)
	}
	if e != 7 {
		t.Errorf("PART-E qty = %d, want 7 (independent scope from PART-D)", e)
	}
}

// TestInventoryDelta_ListBinUOPForNodes_ReturnsAuthoritative pins
// the reconciler-feed contract: the per-bin query returns the
// current authoritative uop_remaining for every bin at the requested
// nodes. Edge's reconciler self-heal reads it to align local cache.
func TestInventoryDelta_ListBinUOPForNodes_ReturnsAuthoritative(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-RECONC", "PART-R", 100)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-R", -7, 1, protocol.ReasonConsumeTick)), "apply delta")

	rows, err := svc.ListBinUOPForNodes([]string{sd.LineNode.Name})
	if err != nil {
		t.Fatalf("list bin uop: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.BinID != bin.ID {
		t.Errorf("BinID = %d, want %d", r.BinID, bin.ID)
	}
	if r.UOPRemaining != 93 {
		t.Errorf("UOPRemaining = %d, want 93 (authoritative; 100 seed - 7 delta)", r.UOPRemaining)
	}
}

// TestInventoryDelta_ListBucketsForStation_FiltersByStation pins
// the per-station scoping: a query for station "A" returns only
// rows whose station column matches "A". Cross-station leakage
// would let Edge see (and mis-attribute) other stations' buckets.
func TestInventoryDelta_ListBucketsForStation_FiltersByStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	apply := func(d *protocol.LinesideBucketDelta) {
		testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(d), "apply")
	}
	// Two stations, two buckets each. Both attribute to nodes the
	// applier can resolve via GetNodeByName.
	stationA := makeBucketDelta(sd.StorageNode.Name, "L1|U1", 100, "PART-A", 5, 1, protocol.ReasonCaptureFill)
	stationA.Station = "STATION-A"
	apply(stationA)
	stationB := makeBucketDelta(sd.LineNode.Name, "L1|U1", 100, "PART-B", 7, 1, protocol.ReasonCaptureFill)
	stationB.Station = "STATION-B"
	apply(stationB)

	rowsA, err := svc.ListBucketsForStation("STATION-A")
	if err != nil {
		t.Fatalf("station A: %v", err)
	}
	if len(rowsA) != 1 || rowsA[0].PartNumber != "PART-A" {
		t.Errorf("station A rows = %+v, want one PART-A row", rowsA)
	}
	rowsB, err := svc.ListBucketsForStation("STATION-B")
	if err != nil {
		t.Fatalf("station B: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].PartNumber != "PART-B" {
		t.Errorf("station B rows = %+v, want one PART-B row", rowsB)
	}
}

// TestApplyBinUOPDelta_CaptureReductionToZeroFiresClearForReuse pins
// the Item 6 manifest-clear trigger: when a capture_reduction delta
// drives uop_remaining to zero, the bin's manifest is cleared and an
// audit row with op=released_capture_empty is written, atomically
// inside the same transaction as the delta apply. Without this
// trigger the bin would sit at uop_remaining=0 with the old manifest
// still attached — visible to the operator UI as "empty" but invisible
// to FindEmptyCompatibleBin.
func TestApplyBinUOPDelta_CaptureReductionToZeroFiresClearForReuse(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CAP-CLEAR", "PART-CC", 25)

	// Apply capture_reduction of -25 → drives the bin to zero.
	d := makeBinDelta(bin.ID, "PART-CC", -25, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(d), "apply capture_reduction")

	// uop_remaining must be 0; payload_code must be cleared (manifest
	// reset by ClearForReuseTx).
	var (
		gotUOP     int
		gotPayload string
	)
	if err := db.QueryRow(`SELECT uop_remaining, payload_code FROM bins WHERE id=$1`,
		bin.ID).Scan(&gotUOP, &gotPayload); err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if gotUOP != 0 {
		t.Errorf("uop_remaining = %d, want 0", gotUOP)
	}
	if gotPayload != "" {
		t.Errorf("payload_code = %q, want empty (manifest must clear on capture_reduction → 0)", gotPayload)
	}

	// Audit row with op=released_capture_empty must exist for this bin.
	var auditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op=$2`,
		bin.ID, audit.OpReleasedCaptureEmpty).Scan(&auditCount); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit count for op=%q = %d, want 1", audit.OpReleasedCaptureEmpty, auditCount)
	}
}

// TestApplyBinUOPDelta_ConsumeTickToZeroDoesNotFireClearForReuse pins
// the negative case: a consume_tick delta that lands on zero must NOT
// fire ClearForReuse. Consume ticks at zero are an overpack signal —
// the PLC counted more parts than the manifest expected, but the bin
// might still physically hold parts. Clearing the manifest on a
// consume tick would erase the operator-set payload while the bin
// still had work to do. Only operator-driven release paths (capture,
// RELEASE EMPTY, partial-back-with-zero) clear the manifest.
func TestApplyBinUOPDelta_ConsumeTickToZeroDoesNotFireClearForReuse(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-TICK-NOCLR", "PART-TNC", 5)

	d := makeBinDelta(bin.ID, "PART-TNC", -5, 1, protocol.ReasonConsumeTick)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(d), "apply consume_tick")

	var (
		gotUOP     int
		gotPayload string
	)
	if err := db.QueryRow(`SELECT uop_remaining, payload_code FROM bins WHERE id=$1`,
		bin.ID).Scan(&gotUOP, &gotPayload); err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if gotUOP != 0 {
		t.Errorf("uop_remaining = %d, want 0", gotUOP)
	}
	if gotPayload != "PART-TNC" {
		t.Errorf("payload_code = %q, want PART-TNC (consume tick must NOT clear manifest)", gotPayload)
	}

	var auditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op=$2`,
		bin.ID, audit.OpReleasedCaptureEmpty).Scan(&auditCount); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditCount != 0 {
		t.Errorf("audit count for op=%q = %d, want 0 (consume tick must NOT trigger manifest clear)",
			audit.OpReleasedCaptureEmpty, auditCount)
	}
}
