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

// R22-1: a reduction for a not-yet-seen bucket no longer clamps to 0 on the
// new-row path — it hits the same CHECK (qty >= 0) rejection as an existing-row
// underflow, so the anomaly surfaces instead of silently drifting the count up.
func TestInventoryDelta_LinesideBucketDelta_RejectsFirstSightNegative(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))
	nodeName := sd.LineNode.Name

	// Negative delta for a part with NO existing bucket — must be rejected, not
	// clamped to a 0 row.
	if err := svc.ApplyLinesideBucketDelta(makeBucketDelta(nodeName, "L3|U3", 300, "PART-C", -7, 1, protocol.ReasonConsumeDrain)); err == nil {
		t.Fatal("expected CHECK violation on a first-sight negative delta, got nil")
	}

	var rowCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lineside_buckets
		WHERE core_node_name=$1 AND part_number='PART-C'`, nodeName).Scan(&rowCount)
	if rowCount != 0 {
		t.Errorf("bucket row count after rejected first-sight negative = %d, want 0 (must not clamp to a 0 row)", rowCount)
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

// TestApplyBinUOPDelta_CaptureReductionOverpackToNegativeFiresClear is the
// bin-18 underflow regression (2026-05-28). Bin at uop=308 receives a
// capture_reduction of -309 (operator overpacked end-of-bin by one): result
// lands at -1, manifest must clear (SME-lock washout), and the audit trail
// must show both the delta row (after_uop=-1) and the clear row (after_uop=0)
// inside the same transaction. Pre-fix the trigger was gated on == 0 so the
// -1 result left payload_code stale and the bin sat misrouted at storage.
func TestApplyBinUOPDelta_CaptureReductionOverpackToNegativeFiresClear(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CAP-NEG-1", "PART-OP", 308)
	preEpoch := bin.DeltaEpoch

	d := makeBinDelta(bin.ID, "PART-OP", -309, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(d), "apply capture_reduction overpack")

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0 (clear ran after delta landed at -1)", got.UOPRemaining)
	}
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (manifest cleared on overpack)", got.PayloadCode)
	}
	if got.Manifest != nil {
		t.Errorf("Manifest = %v, want nil", got.Manifest)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed = true, want false")
	}
	if got.DeltaEpoch <= preEpoch {
		t.Errorf("DeltaEpoch = %d, want > %d (clear must bump epoch)", got.DeltaEpoch, preEpoch)
	}

	// Audit trail: must contain both the bin_uop_delta row with after_uop=-1
	// AND the released_capture_empty row with after_uop=0.
	var deltaCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op='bin_uop_delta' AND after_uop=-1`, bin.ID).Scan(&deltaCount); err != nil {
		t.Fatalf("read delta audit: %v", err)
	}
	if deltaCount != 1 {
		t.Errorf("bin_uop_delta audit rows with after_uop=-1 = %d, want 1", deltaCount)
	}
	var clearCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op=$2 AND after_uop=0`,
		bin.ID, audit.OpReleasedCaptureEmpty).Scan(&clearCount); err != nil {
		t.Fatalf("read clear audit: %v", err)
	}
	if clearCount != 1 {
		t.Errorf("released_capture_empty audit rows with after_uop=0 = %d, want 1", clearCount)
	}
}

// TestApplyBinUOPDelta_CaptureReductionLargerNegativeFiresClear extends the
// overpack case past -1. A delta landing at -5 must still fire the clear —
// the <= 0 widening is not bounded by the magnitude of the negative.
func TestApplyBinUOPDelta_CaptureReductionLargerNegativeFiresClear(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CAP-NEG-5", "PART-OP5", 100)

	d := makeBinDelta(bin.ID, "PART-OP5", -105, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(d), "apply capture_reduction -105")

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 0 || got.PayloadCode != "" {
		t.Errorf("post-clear state: uop=%d payload=%q, want 0/'' (clear must fire at -5)",
			got.UOPRemaining, got.PayloadCode)
	}
}

// TestApplyBinUOPDelta_ConsumeTickThenCaptureReductionClears is the realistic
// multi-step sequence (case E2): PLC overshoots and drains the runtime cache
// past zero (consume_tick must NOT clear), then the operator releases with
// one more part captured. The capture_reduction lands further negative and
// must clear, even though the bin was already below zero when the delta
// arrived.
func TestApplyBinUOPDelta_ConsumeTickThenCaptureReductionClears(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-OS-CAP", "PART-OSC", 100)

	// PLC overshoot: drains to -10. Must NOT clear.
	testutil.MustNoErr(t,
		svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-OSC", -110, 1, protocol.ReasonConsumeTick)),
		"consume_tick overshoot")
	mid, _ := db.GetBin(bin.ID)
	if mid.UOPRemaining != -10 {
		t.Errorf("after consume_tick: UOPRemaining = %d, want -10", mid.UOPRemaining)
	}
	if mid.PayloadCode != "PART-OSC" {
		t.Errorf("after consume_tick: PayloadCode = %q, want PART-OSC (tick must NOT clear)", mid.PayloadCode)
	}

	// Operator release with one more part captured: -10 + -1 = -11. Must clear.
	testutil.MustNoErr(t,
		svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-OSC", -1, 2, protocol.ReasonCaptureReduction)),
		"capture_reduction post-overshoot")
	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 0 {
		t.Errorf("after capture: UOPRemaining = %d, want 0 (clear must fire on capture landing at -11)", got.UOPRemaining)
	}
	if got.PayloadCode != "" {
		t.Errorf("after capture: PayloadCode = %q, want empty", got.PayloadCode)
	}
}

// TestApplyBinUOPDelta_CaptureReductionFromOneToZeroBoundary pins case F: a
// capture landing exactly on zero from uop=1 still fires the clear (the
// boundary the original == 0 condition was written for; the widening to <= 0
// must not regress this).
func TestApplyBinUOPDelta_CaptureReductionFromOneToZeroBoundary(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-BNDRY", "PART-BND", 1)

	testutil.MustNoErr(t,
		svc.ApplyBinUOPDelta(makeBinDelta(bin.ID, "PART-BND", -1, 1, protocol.ReasonCaptureReduction)),
		"capture_reduction boundary")

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 0 || got.PayloadCode != "" {
		t.Errorf("boundary state: uop=%d payload=%q, want 0/''", got.UOPRemaining, got.PayloadCode)
	}
}

// TestApplyBinUOPDelta_CaptureReductionReplayShortCircuits pins G1: replay
// of a previously-applied SequenceID is skipped by dedup before the trigger
// condition is evaluated. The first apply fires the clear normally; the
// replay must not double-write the audit row or further bump the epoch.
func TestApplyBinUOPDelta_CaptureReductionReplayShortCircuits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-REPLAY", "PART-RP", 50)

	d := makeBinDelta(bin.ID, "PART-RP", -55, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(d), "first apply")

	afterFirst, _ := db.GetBin(bin.ID)

	if err := svc.ApplyBinUOPDelta(d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("replay error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}

	afterReplay, _ := db.GetBin(bin.ID)
	if afterReplay.DeltaEpoch != afterFirst.DeltaEpoch {
		t.Errorf("DeltaEpoch after replay = %d, want %d (replay must not bump epoch)",
			afterReplay.DeltaEpoch, afterFirst.DeltaEpoch)
	}

	var clearCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op=$2`,
		bin.ID, audit.OpReleasedCaptureEmpty).Scan(&clearCount); err != nil {
		t.Fatalf("read clear audit: %v", err)
	}
	if clearCount != 1 {
		t.Errorf("released_capture_empty rows after replay = %d, want 1 (no double-write)", clearCount)
	}
}

// TestApplyBinUOPDelta_CaptureReductionZeroOnEmptyBinIsIdempotent pins G2: a
// fresh-sequence delta=0 capture_reduction against an already-empty bin
// passes dedup, evaluates 0 + 0 = 0 (the <= 0 branch fires), and ClearForReuse
// runs idempotently. The redundant audit row is acceptable — the
// alternative is a clamp on already-clean state, which the SME lock forbids.
func TestApplyBinUOPDelta_CaptureReductionZeroOnEmptyBinIsIdempotent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db))

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-IDEMP", "", 0)
	preEpoch := bin.DeltaEpoch

	d := makeBinDelta(bin.ID, "", 0, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(d), "apply capture_reduction=0 on empty bin")

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 0 || got.PayloadCode != "" {
		t.Errorf("post-state: uop=%d payload=%q, want 0/'' (clear was idempotent)",
			got.UOPRemaining, got.PayloadCode)
	}
	if got.DeltaEpoch <= preEpoch {
		t.Errorf("DeltaEpoch = %d, want > %d (clear still bumps epoch even when no-op semantically)",
			got.DeltaEpoch, preEpoch)
	}
}
