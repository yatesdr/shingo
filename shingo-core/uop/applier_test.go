//go:build docker

package uop_test

import (
	"encoding/json"
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

// testStation is the envelope source every apply is attributed to. It is an
// ARGUMENT now, not a payload field: the station moved off the delta body and
// onto the envelope, so the applier takes what the transport carried instead of
// what the sender asserted.
const testStation = "ALN_001"

func makeBinDelta(binID int64, payloadCode string, delta int, seq int64, reason protocol.BinUOPDeltaReason) *protocol.BinUOPDelta {
	now := time.Now().UTC()
	return &protocol.BinUOPDelta{
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-1", "PART-A", 100)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-A", -3, 1, protocol.ReasonConsumeTick)), "apply consume_tick")
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-A", -2, 2, protocol.ReasonConsumeTick)), "apply consume_tick #2")

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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-DUP", "PART-A", 100)

	d := makeBinDelta(bin.ID, "PART-A", -10, 5, protocol.ReasonConsumeTick)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply first time")
	// Replay the exact same envelope.
	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("replay error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}
	// And a re-replay just to be sure the second skip didn't advance state.
	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("third replay error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}

	var got int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got), "read bin")
	if got != 90 {
		t.Errorf("uop_remaining = %d, want 90 (100 - 10 once, not 100 - 30)", got)
	}
}

// TestInventoryDelta_BinUOPDelta_StaleEpochDroppedAndAudited verifies that a
// delta whose wire epoch is below the bin's current delta_epoch (and >0) belongs
// to a retired delta-stream generation — the bin was reset on Core after Edge
// cached the old epoch. It must be dropped (uop_remaining unchanged) and recorded
// as a stale_epoch_dropped observation row so the dropped quantity surfaces in the
// discrepancy view instead of vanishing.
func TestInventoryDelta_BinUOPDelta_StaleEpochDroppedAndAudited(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-STALE-EPOCH", "PART-A", 100)
	// Advance the bin to epoch 2 (a load/clear/release bumped it on Core).
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "advance epoch")

	// A consume tick still carrying the retired epoch 1 (>0) must be dropped.
	d := makeBinDelta(bin.ID, "PART-A", -7, 1, protocol.ReasonConsumeTick)
	d.Epoch = 1
	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("stale-epoch apply error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}

	var got int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got), "read bin")
	if got != 100 {
		t.Errorf("uop_remaining = %d, want 100 unchanged (stale delta must not apply)", got)
	}

	var before, after int
	var meta string
	testutil.MustNoErr(t, db.QueryRow(`SELECT before_uop, after_uop, metadata FROM bin_uop_ledger
		WHERE bin_id=$1 AND op='stale_epoch_dropped'`, bin.ID).Scan(&before, &after, &meta), "read stale audit row")
	if before != 100 || after != 100 {
		t.Errorf("audit before/after = %d/%d, want 100/100 (count-unchanged observation)", before, after)
	}
	var m struct {
		Delta     int   `json:"delta"`
		WireEpoch int64 `json:"wire_epoch"`
		BinEpoch  int64 `json:"bin_epoch"`
	}
	testutil.MustNoErr(t, json.Unmarshal([]byte(meta), &m), "parse audit metadata")
	if m.Delta != -7 || m.WireEpoch != 1 || m.BinEpoch != 2 {
		t.Errorf("audit metadata = %+v, want delta=-7 wire_epoch=1 bin_epoch=2", m)
	}
}

// TestInventoryDelta_BinUOPDelta_BootstrapEpochZeroApplies pins the >0 clause:
// epoch 0 is the bootstrap/unknown sentinel (Edge restart, fresh runtime, the
// ADD-COLUMN backfill) and must always apply even though the bin is at epoch 1.
func TestInventoryDelta_BinUOPDelta_BootstrapEpochZeroApplies(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-EPOCH0", "PART-A", 100)
	// makeBinDelta leaves Epoch == 0; the bin defaults to delta_epoch 1.
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-A", -4, 1, protocol.ReasonConsumeTick)), "apply epoch-0 delta")

	var got int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got), "read bin")
	if got != 96 {
		t.Errorf("uop_remaining = %d, want 96 (epoch-0 bootstrap delta must apply)", got)
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-ORD", "PART-A", 100)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-A", -5, 10, protocol.ReasonConsumeTick)), "apply seq=10")
	if err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-A", -7, 5, protocol.ReasonConsumeTick)); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DELTA-MIS", "PART-A", 100)

	err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-DIFFERENT", -1, 1, protocol.ReasonConsumeTick))
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	if err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(999999999, "PART-A", -1, 1, protocol.ReasonConsumeTick)); err == nil {
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	// Round-3 Obs 8: applier validates core_node_name resolves to a
	// Core node row, so the fixture's storage node is what the delta
	// must reference. SetupStandardData creates STORAGE-A1.
	nodeName := sd.StorageNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation, makeBucketDelta(nodeName, "L1|U1", 100, "PART-A", 47, 1, protocol.ReasonCaptureFill)), "capture_fill")

	var qty int
	if err := db.QueryRow(`SELECT qty FROM lineside_buckets
		WHERE station='ALN_001' AND core_node_name=$1 AND pair_key='L1|U1' AND style_id=100 AND part_number='PART-A'`, nodeName).
		Scan(&qty); err != nil {
		t.Fatalf("read bucket after fill: %v", err)
	}
	if qty != 47 {
		t.Errorf("bucket qty after fill = %d, want 47", qty)
	}

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation, makeBucketDelta(nodeName, "L1|U1", 100, "PART-A", -47, 2, protocol.ReasonConsumeDrain)), "consume_drain to zero")

	var rowCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lineside_buckets
		WHERE station='ALN_001' AND core_node_name=$1 AND pair_key='L1|U1' AND style_id=100 AND part_number='PART-A'`, nodeName).
		Scan(&rowCount)
	if rowCount != 0 {
		t.Errorf("bucket row count at zero = %d, want 0 (Option C — empty buckets are deleted)", rowCount)
	}
}

// TestInventoryDelta_LinesideBucketOneNodeOneRowAcrossStations is the v65
// identity prerequisite, expressed as behaviour rather than as a constraint
// definition.
//
// ONE PHYSICAL BUCKET IS ONE ROW NO MATTER WHICH EDGE REPORTS IT. The row
// describes parts sitting at a Core node; which Pi noticed them is not part of
// where they are. With `station` in the uniqueness key the same physical
// bucket reported by two edges became TWO rows, and the damage was not the
// duplication — it was that the duplication is invisible from the read side:
//
//   - SystemUOPForPayload's bucket term (service/inventory_system_count.go)
//     groups by payload with NO station predicate, so it sums both rows.
//   - An inflated on-hand total sits ABOVE the replenishment threshold, so no
//     replenishment is requested. That is the Springfield 74576 shape — a
//     stranded 250-qty bucket holding the total up — reached by a second
//     route, and this one has no stranded bucket to find.
//
// The reason this is a PREREQUISITE and not a bug report: `station` is
// one-valued per plant today (Springfield: one edge_registry row, one distinct
// value across every station-bearing table), so the duplicate cannot currently
// be constructed in production. Per-edge identity constructs it. The test uses
// two station strings to reach the state identity is about to make reachable.
func TestInventoryDelta_LinesideBucketOneNodeOneRowAcrossStations(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
	nodeName := sd.StorageNode.Name

	// The SAME physical bucket — same node, pair, style, part — reported by two
	// different edges. Distinct SequenceIDs are not enough to keep these apart
	// and are not meant to be: claimDeltaSequence scopes the at-most-once guard
	// BY STATION, so both deltas legitimately apply. That is correct, and it is
	// the other family — see ApplyLinesideBucketDelta's doc comment.
	first := makeBucketDelta(nodeName, "L1|U1", 100, "PART-A", 30, 1, protocol.ReasonCaptureFill)
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta("PLANT.EDGE-1", first), "edge-1 capture_fill")

	second := makeBucketDelta(nodeName, "L1|U1", 100, "PART-A", 12, 1, protocol.ReasonCaptureFill)
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta("PLANT.EDGE-2", second), "edge-2 capture_fill")

	var rows, qty int
	var station string
	if err := db.QueryRow(`SELECT count(*), COALESCE(SUM(qty),0), COALESCE(MAX(station),'')
		FROM lineside_buckets
		WHERE core_node_name=$1 AND pair_key='L1|U1' AND style_id=100 AND part_number='PART-A'`,
		nodeName).Scan(&rows, &qty, &station); err != nil {
		t.Fatalf("read bucket: %v", err)
	}
	if rows != 1 {
		t.Errorf("one physical bucket reported by two edges produced %d rows, want 1.\n"+
			"SystemUOPForPayload sums buckets with no station predicate, so a second row is a "+
			"silent overcount of on-hand inventory, and an overcount SUPPRESSES replenishment.", rows)
	}
	if qty != 42 {
		t.Errorf("bucket qty = %d, want 42 (30 + 12 — both deltas are real observations "+
			"of one bucket and both must land on the one row)", qty)
	}
	// The column survives as attribute data: last reporter wins, and that is
	// all it claims to be now.
	if station != "PLANT.EDGE-2" {
		t.Errorf("station = %q, want the last reporter PLANT.EDGE-2 — the column is "+
			"attribute data now, not identity, and a stale value would misreport who last saw it", station)
	}

	// And the zero-GC has to be station-free too, or the edge that empties a
	// bucket is not the edge whose name is on the row and the DELETE matches
	// nothing — leaving a qty=0 orphan that never clears.
	drain := makeBucketDelta(nodeName, "L1|U1", 100, "PART-A", -42, 2, protocol.ReasonConsumeDrain)
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta("PLANT.EDGE-1", drain), "edge-1 drains what edge-2 last touched")

	_ = db.QueryRow(`SELECT count(*) FROM lineside_buckets
		WHERE core_node_name=$1 AND pair_key='L1|U1' AND style_id=100 AND part_number='PART-A'`,
		nodeName).Scan(&rows)
	if rows != 0 {
		t.Errorf("emptied bucket left %d row(s), want 0 — a station-scoped GC cannot delete a row "+
			"another edge last stamped, so the orphan would linger at qty=0", rows)
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
	nodeName := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation, makeBucketDelta(nodeName, "L2|U2", 200, "PART-B", 5, 1, protocol.ReasonCaptureFill)), "capture_fill")
	// Try to drain 10 from a bucket that holds 5.
	if err := svc.ApplyLinesideBucketDelta(testStation, makeBucketDelta(nodeName, "L2|U2", 200, "PART-B", -10, 2, protocol.ReasonConsumeDrain)); err == nil {
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
	nodeName := sd.LineNode.Name

	// Negative delta for a part with NO existing bucket — must be rejected, not
	// clamped to a 0 row.
	if err := svc.ApplyLinesideBucketDelta(testStation, makeBucketDelta(nodeName, "L3|U3", 300, "PART-C", -7, 1, protocol.ReasonConsumeDrain)); err == nil {
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
	nodeName := sd.StorageNode.Name

	d := makeBucketDelta(nodeName, "L1|U1", 300, "PART-C", 10, 1, protocol.ReasonCaptureFill)
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation, d), "first apply")
	if err := svc.ApplyLinesideBucketDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
	nodeName := sd.LineNode.Name

	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation, makeBucketDelta(nodeName, "L1|U1", 400, "PART-D", 5, 1, protocol.ReasonCaptureFill)), "part D apply")
	// Same SequenceID, different part — this is a separate scope.
	if err := svc.ApplyLinesideBucketDelta(testStation, makeBucketDelta(nodeName, "L1|U1", 400, "PART-E", 7, 1, protocol.ReasonCaptureFill)); err != nil {
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-RECONC", "PART-R", 100)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-R", -7, 1, protocol.ReasonConsumeTick)), "apply delta")

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

// TestInventoryDelta_ListBucketsForNodes_FiltersByNodeNotByReporter is the
// sixth station-keyed site, and the assertion is the one the old test could
// not make.
//
// The old test pinned "a query for station A returns only rows whose station
// column says A" — which was true, and was the bug. The caller is Edge's drift
// reconciliation asking "what is authoritative at the nodes I OWN"; after v65
// the station column is the LAST REPORTER. Those two answers coincide only
// while every edge shares one station string, and the identity change is
// precisely what ends that.
//
// The case below is the one that used to go silently wrong: a bucket at edge
// A's node whose most recent delta came from edge B. Under the station filter
// it vanished from A's reconciliation — and an empty result and a clean result
// are indistinguishable to the caller, so the drift detector would report no
// drift by failing to look.
func TestInventoryDelta_ListBucketsForNodes_FiltersByNodeNotByReporter(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	nodeA, nodeB := sd.StorageNode.Name, sd.LineNode.Name

	// A bucket at nodeA, reported by EDGE-1.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta("PLANT.EDGE-1",
		makeBucketDelta(nodeA, "L1|U1", 100, "PART-A", 5, 1, protocol.ReasonCaptureFill)), "edge-1 at nodeA")
	// A bucket at nodeB, reported by EDGE-2.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta("PLANT.EDGE-2",
		makeBucketDelta(nodeB, "L1|U1", 100, "PART-B", 7, 1, protocol.ReasonCaptureFill)), "edge-2 at nodeB")
	// AND THE CASE THAT BREAKS THE OLD FILTER: EDGE-2 touches nodeA's bucket
	// last, so the row now carries EDGE-2 as its reporter while the parts are
	// still sitting at a node EDGE-1 owns.
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta("PLANT.EDGE-2",
		makeBucketDelta(nodeA, "L1|U1", 100, "PART-A", 3, 1, protocol.ReasonCaptureFill)), "edge-2 touches nodeA last")

	rowsA, err := svc.ListBucketsForNodes([]string{nodeA})
	if err != nil {
		t.Fatalf("nodeA: %v", err)
	}
	if len(rowsA) != 1 || rowsA[0].PartNumber != "PART-A" {
		t.Fatalf("nodeA rows = %+v, want one PART-A row. A bucket at a node this edge owns must "+
			"be visible to its reconciliation no matter which edge last reported it — filtering "+
			"by the reporter is how the drift detector stops seeing drift.", rowsA)
	}
	if rowsA[0].Qty != 8 {
		t.Errorf("nodeA qty = %d, want 8 (5 + 3, both observations of one physical bucket)", rowsA[0].Qty)
	}

	rowsB, err := svc.ListBucketsForNodes([]string{nodeB})
	if err != nil {
		t.Fatalf("nodeB: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].PartNumber != "PART-B" {
		t.Errorf("nodeB rows = %+v, want one PART-B row", rowsB)
	}

	// Both at once, which is the shape the real caller sends.
	both, err := svc.ListBucketsForNodes([]string{nodeA, nodeB})
	if err != nil {
		t.Fatalf("both nodes: %v", err)
	}
	if len(both) != 2 {
		t.Errorf("both-node query returned %d rows, want 2", len(both))
	}

	// An empty node set returns nothing rather than everything — the caller
	// that sends no nodes is asking about no nodes.
	if rows, err := svc.ListBucketsForNodes(nil); err != nil || len(rows) != 0 {
		t.Errorf("empty node set: rows=%d err=%v, want 0/nil", len(rows), err)
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CAP-CLEAR", "PART-CC", 25)

	// Apply capture_reduction of -25 → drives the bin to zero.
	d := makeBinDelta(bin.ID, "PART-CC", -25, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply capture_reduction")

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
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-TICK-NOCLR", "PART-TNC", 5)

	d := makeBinDelta(bin.ID, "PART-TNC", -5, 1, protocol.ReasonConsumeTick)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply consume_tick")

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
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CAP-NEG-1", "PART-OP", 308)
	preEpoch := bin.DeltaEpoch

	d := makeBinDelta(bin.ID, "PART-OP", -309, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply capture_reduction overpack")

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
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger
		WHERE bin_id=$1 AND op='bin_uop_delta' AND after_uop=-1`, bin.ID).Scan(&deltaCount); err != nil {
		t.Fatalf("read delta audit: %v", err)
	}
	if deltaCount != 1 {
		t.Errorf("bin_uop_delta audit rows with after_uop=-1 = %d, want 1", deltaCount)
	}
	var clearCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CAP-NEG-5", "PART-OP5", 100)

	d := makeBinDelta(bin.ID, "PART-OP5", -105, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply capture_reduction -105")

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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-OS-CAP", "PART-OSC", 100)

	// PLC overshoot: drains to -10. Must NOT clear.
	testutil.MustNoErr(t,
		svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-OSC", -110, 1, protocol.ReasonConsumeTick)),
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
		svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-OSC", -1, 2, protocol.ReasonCaptureReduction)),
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-BNDRY", "PART-BND", 1)

	testutil.MustNoErr(t,
		svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-BND", -1, 1, protocol.ReasonCaptureReduction)),
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-REPLAY", "PART-RP", 50)

	d := makeBinDelta(bin.ID, "PART-RP", -55, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "first apply")

	afterFirst, _ := db.GetBin(bin.ID)

	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Errorf("replay error = %v, want uop.ErrInventoryDeltaSkipped", err)
	}

	afterReplay, _ := db.GetBin(bin.ID)
	if afterReplay.DeltaEpoch != afterFirst.DeltaEpoch {
		t.Errorf("DeltaEpoch after replay = %d, want %d (replay must not bump epoch)",
			afterReplay.DeltaEpoch, afterFirst.DeltaEpoch)
	}

	var clearCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger
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
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-IDEMP", "", 0)
	preEpoch := bin.DeltaEpoch

	d := makeBinDelta(bin.ID, "", 0, 1, protocol.ReasonCaptureReduction)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply capture_reduction=0 on empty bin")

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

// TestApplyBinUOPDelta_FirstDeltaBindsBlankProduceBin pins the routine
// half of produce-tick identity binding: the designed blank fresh carrier
// takes its payload from the first produce tick that lands on it, the
// count applies in the same tx, a payload_bound_first_delta observation
// row records the bind, and — load-bearing — the delta_epoch does NOT
// move (there is no retired count stream to fence; a bump would open the
// stale-epoch drop window observed live at HK 2026-07-16 14:02Z).
func TestApplyBinUOPDelta_FirstDeltaBindsBlankProduceBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-BIND-BLANK", "", 0)
	preEpoch := bin.DeltaEpoch

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(bin.ID, "PART-NEW", 3, 1, protocol.ReasonProduceTick)), "apply first produce tick")

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "PART-NEW" {
		t.Errorf("PayloadCode = %q, want %q (first delta binds identity)", got.PayloadCode, "PART-NEW")
	}
	if got.UOPRemaining != 3 {
		t.Errorf("UOPRemaining = %d, want 3 (count applies with the bind)", got.UOPRemaining)
	}
	if got.DeltaEpoch != preEpoch {
		t.Errorf("DeltaEpoch = %d, want %d unchanged (bind must NOT bump epoch)", got.DeltaEpoch, preEpoch)
	}

	var old string
	testutil.MustNoErr(t, db.QueryRow(`SELECT metadata->>'old_payload' FROM bin_uop_ledger
		WHERE bin_id=$1 AND op=$2`, bin.ID, audit.OpPayloadBoundFirstDelta).Scan(&old), "read bind audit row")
	if old != "" {
		t.Errorf("audit old_payload = %q, want blank (bin was a fresh carrier)", old)
	}

	var anomaly bool
	_ = db.QueryRow(`SELECT anomaly_at IS NOT NULL FROM bins WHERE id=$1`, bin.ID).Scan(&anomaly)
	if anomaly {
		t.Error("anomaly flagged on a routine zero-count bind; want unflagged")
	}
}

// TestApplyBinUOPDelta_FirstDeltaRebindsStaleLabelAtZero pins the HK
// 2026-07-16 case directly: a hand-typed stale label on a zero-count
// carrier is overwritten by the first produce tick — the count applies
// instead of freezing, and the audit row preserves the old label.
func TestApplyBinUOPDelta_FirstDeltaRebindsStaleLabelAtZero(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-BIND-STALE", "PART-OLD", 0)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(bin.ID, "PART-NEW", 2, 1, protocol.ReasonProduceTick)), "apply produce tick over stale label")

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "PART-NEW" || got.UOPRemaining != 2 {
		t.Errorf("post-state payload=%q uop=%d, want PART-NEW/2 (stale label rebinds, count applies)",
			got.PayloadCode, got.UOPRemaining)
	}

	var old string
	testutil.MustNoErr(t, db.QueryRow(`SELECT metadata->>'old_payload' FROM bin_uop_ledger
		WHERE bin_id=$1 AND op=$2`, bin.ID, audit.OpPayloadBoundFirstDelta).Scan(&old), "read bind audit row")
	if old != "PART-OLD" {
		t.Errorf("audit old_payload = %q, want PART-OLD", old)
	}
}

// TestApplyBinUOPDelta_ProduceRebindWithInventoryKeepsCounting pins the
// mixed-contents rule: a produce tick against a bin still holding units
// under another label RELABELS the bin, KEEPS COUNTING (the tote's unit
// total stays correct), flags the anomaly for a later cycle count, and
// records the old label + units aboard in a
// payload_rebound_with_inventory observation row. A produce count must
// never freeze on a label disagreement.
func TestApplyBinUOPDelta_ProduceRebindWithInventoryKeepsCounting(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-REBIND-INV", "PART-OLD", 480)
	preEpoch := bin.DeltaEpoch

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(bin.ID, "PART-NEW", 3, 1, protocol.ReasonProduceTick)), "apply produce tick with inventory aboard")

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "PART-NEW" || got.UOPRemaining != 483 {
		t.Errorf("post-state payload=%q uop=%d, want PART-NEW/483 (rebind + count continues)",
			got.PayloadCode, got.UOPRemaining)
	}
	if got.DeltaEpoch != preEpoch {
		t.Errorf("DeltaEpoch = %d, want %d unchanged (rebind must NOT bump epoch)", got.DeltaEpoch, preEpoch)
	}

	var anomaly bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT anomaly_at IS NOT NULL FROM bins WHERE id=$1`, bin.ID).Scan(&anomaly), "read anomaly flag")
	if !anomaly {
		t.Error("anomaly not flagged; mixed-contents rebind must mark the bin for a cycle count")
	}

	var old string
	var aboard int
	testutil.MustNoErr(t, db.QueryRow(`SELECT metadata->>'old_payload', (metadata->>'inventory_at_rebind')::int
		FROM bin_uop_ledger WHERE bin_id=$1 AND op=$2`, bin.ID, audit.OpPayloadReboundWithInventory).Scan(&old, &aboard), "read rebind audit row")
	if old != "PART-OLD" || aboard != 480 {
		t.Errorf("rebind audit old=%q aboard=%d, want PART-OLD/480", old, aboard)
	}

	// A second tick is now a clean match — no second rebind row.
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(bin.ID, "PART-NEW", 2, 2, protocol.ReasonProduceTick)), "apply follow-up tick")
	var rebindRows int
	_ = db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op=$2`,
		bin.ID, audit.OpPayloadReboundWithInventory).Scan(&rebindRows)
	if rebindRows != 1 {
		t.Errorf("rebind audit rows = %d, want 1 (follow-up ticks match the new label)", rebindRows)
	}
}

// TestApplyBinUOPDelta_ConsumeMismatchStillRejectsButLoudly pins two
// things: (1) the ALN_001 guard is intact for non-produce reasons — a
// consume drain never lands on inventory it doesn't describe and never
// relabels the bin; (2) the drop is no longer silent — the bin is
// anomaly-flagged and each dropped delta writes a
// payload_mismatch_dropped observation row carrying the dropped
// quantity, so the discrepancy ledger can reconstruct the loss. The
// dedup seq must stay unconsumed (tx rollback), so the same envelope
// applies cleanly after the label is corrected.
func TestApplyBinUOPDelta_ConsumeMismatchStillRejectsButLoudly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CONS-MIS", "PART-OLD", 100)

	d := makeBinDelta(bin.ID, "PART-NEW", -5, 1, protocol.ReasonConsumeTick)
	if err := svc.ApplyBinUOPDelta(testStation, d); err == nil {
		t.Fatal("expected consume payload-mismatch error, got nil")
	}
	if err := svc.ApplyBinUOPDelta(testStation, makeBinDelta(bin.ID, "PART-NEW", -3, 2, protocol.ReasonConsumeTick)); err == nil {
		t.Fatal("expected second consume payload-mismatch error, got nil")
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "PART-OLD" || got.UOPRemaining != 100 {
		t.Errorf("post-state payload=%q uop=%d, want PART-OLD/100 (consume never rebinds or applies on mismatch)",
			got.PayloadCode, got.UOPRemaining)
	}

	var anomaly bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT anomaly_at IS NOT NULL FROM bins WHERE id=$1`, bin.ID).Scan(&anomaly), "read anomaly flag")
	if !anomaly {
		t.Error("anomaly not flagged; rejected deltas must be visible on the bins page")
	}

	var droppedRows int
	var droppedSum int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*), COALESCE(SUM((metadata->>'delta')::int), 0)
		FROM bin_uop_ledger WHERE bin_id=$1 AND op=$2`,
		bin.ID, audit.OpPayloadMismatchDropped).Scan(&droppedRows, &droppedSum), "read dropped observation rows")
	if droppedRows != 2 || droppedSum != -8 {
		t.Errorf("dropped rows/sum = %d/%d, want 2/-8 (one observation per drop, quantities reconstructible)",
			droppedRows, droppedSum)
	}

	// Fix the label; the ORIGINAL envelope must now apply — proof the
	// rejected attempt rolled back without consuming its dedup seq.
	_, err := db.Exec(`UPDATE bins SET payload_code='PART-NEW' WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "correct label")
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "replay original envelope after label fix")
	got2, _ := db.GetBin(bin.ID)
	if got2.UOPRemaining != 95 {
		t.Errorf("UOPRemaining after replay = %d, want 95", got2.UOPRemaining)
	}
}

// TestApplyBinUOPDelta_ConsumeTickNeverBindsBlankBin pins the produce-only
// scope of identity binding: a consume tick against a blank bin applies
// (blank matches anything — unchanged behavior) but must NOT write a
// label. A consume bin arriving blank is an anomaly worth seeing, not one
// to paper over.
func TestApplyBinUOPDelta_ConsumeTickNeverBindsBlankBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CONS-BLANK", "", 0)

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(bin.ID, "PART-A", -1, 1, protocol.ReasonConsumeTick)), "apply consume tick on blank bin")

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want blank (consume never binds identity)", got.PayloadCode)
	}
	if got.UOPRemaining != -1 {
		t.Errorf("UOPRemaining = %d, want -1 (blank passes the guard, delta applies)", got.UOPRemaining)
	}
}

// TestInventoryDelta_C6_AnomalyObservability pins the P2-C6 observability
// surfaces: a stale-epoch drop now flags the bin's anomaly_at (today only the
// payload-mismatch drop did) and bumps a reason-split counter; a payload-mismatch
// drop bumps its own counter; and AnomalySummary rolls up the counters plus the
// live counts of anomaly-flagged bins and bins staged past their own TTL. All
// display/counters — no apply behavior changes.
func TestInventoryDelta_C6_AnomalyObservability(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})

	// (1) Stale-epoch drop: bin at epoch 2, a delta carrying retired epoch 1.
	staleBin := createTestBin(t, db, sd.StorageNode.ID, "BIN-C6-STALE", "PART-A", 100)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2 WHERE id=$1`, staleBin.ID)
	testutil.MustNoErr(t, err, "advance epoch")
	sd1 := makeBinDelta(staleBin.ID, "PART-A", -7, 1, protocol.ReasonConsumeTick)
	sd1.Epoch = 1
	if err := svc.ApplyBinUOPDelta(testStation, sd1); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("stale-epoch apply = %v, want ErrInventoryDeltaSkipped", err)
	}
	staleGot, _ := db.GetBin(staleBin.ID)
	if staleGot.AnomalyAt == nil {
		t.Error("stale-epoch drop must now flag anomaly_at (P2-C6)")
	}

	// (2) Payload-mismatch drop: consume delta whose payload differs from the bin.
	mismatchBin := createTestBin(t, db, sd.StorageNode.ID, "BIN-C6-MISMATCH", "PART-A", 50)
	if err := svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(mismatchBin.ID, "PART-WRONG", -3, 1, protocol.ReasonConsumeTick)); err == nil {
		t.Fatal("payload-mismatch delta should have been rejected")
	}
	mismatchGot, _ := db.GetBin(mismatchBin.ID)
	if mismatchGot.AnomalyAt == nil {
		t.Error("payload-mismatch drop must flag anomaly_at")
	}

	// (3) A bin parked staged past its own TTL.
	staleStaged := createTestBin(t, db, sd.StorageNode.ID, "BIN-C6-STAGED", "PART-A", 20)
	_, err = db.Exec(`UPDATE bins SET status='staged', staged_at=NOW() - interval '3 hours',
		staged_expires_at=NOW() - interval '1 hour' WHERE id=$1`, staleStaged.ID)
	testutil.MustNoErr(t, err, "stage past ttl")

	// Reason-split counters (per-service, deterministic).
	stale, mismatch := svc.DroppedDeltaCounts()
	if stale != 1 {
		t.Errorf("droppedStaleEpoch = %d, want 1", stale)
	}
	if mismatch != 1 {
		t.Errorf("droppedPayloadMismatch = %d, want 1", mismatch)
	}

	// AnomalySummary rolls it all up.
	sum, err := svc.AnomalySummary()
	testutil.MustNoErr(t, err, "anomaly summary")
	if sum.DroppedStaleEpoch != 1 || sum.DroppedPayloadMismatch != 1 {
		t.Errorf("summary drop counters = %d/%d, want 1/1", sum.DroppedStaleEpoch, sum.DroppedPayloadMismatch)
	}
	if sum.RejectedDeltaBins < 2 {
		t.Errorf("RejectedDeltaBins = %d, want >= 2 (stale-epoch + payload-mismatch bins)", sum.RejectedDeltaBins)
	}
	if sum.StaleStagedBins < 1 {
		t.Errorf("StaleStagedBins = %d, want >= 1 (the bin staged past its TTL)", sum.StaleStagedBins)
	}

	// RejectedDeltaDetail is the drill-down behind the count: WHICH carrier, part,
	// node, and why — so the operator knows what to cycle-count. Both dropped bins
	// appear with their reason; the stale-staged bin (not anomaly-flagged) does not.
	detail, err := svc.RejectedDeltaDetail()
	testutil.MustNoErr(t, err, "rejected-delta detail")
	byLabel := map[string]uop.RejectedDeltaBin{}
	for _, b := range detail {
		byLabel[b.BinLabel] = b
	}
	staleRow, ok := byLabel["BIN-C6-STALE"]
	if !ok {
		t.Fatal("stale-epoch bin missing from rejected-delta detail")
	}
	if staleRow.Reason != "stale_epoch_dropped" {
		t.Errorf("stale bin reason = %q, want stale_epoch_dropped", staleRow.Reason)
	}
	if staleRow.DropCount < 1 {
		t.Errorf("stale bin drop_count = %d, want >= 1", staleRow.DropCount)
	}
	if staleRow.PayloadCode != "PART-A" || staleRow.NodeName == "" {
		t.Errorf("stale bin detail = %+v, want part PART-A and a resolved node name", staleRow)
	}
	if staleRow.LastReject == nil {
		t.Error("stale bin last_reject_at should be set (a drop was audited)")
	}
	mmRow, ok := byLabel["BIN-C6-MISMATCH"]
	if !ok {
		t.Fatal("payload-mismatch bin missing from rejected-delta detail")
	}
	if mmRow.Reason != "payload_mismatch_dropped" {
		t.Errorf("mismatch bin reason = %q, want payload_mismatch_dropped", mmRow.Reason)
	}
	if _, present := byLabel["BIN-C6-STAGED"]; present {
		t.Error("stale-staged bin must NOT appear in rejected-delta detail (it is not anomaly-flagged)")
	}
}
