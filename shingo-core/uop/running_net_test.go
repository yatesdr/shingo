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
	"shingocore/store"
	"shingocore/uop"
)

// running_net_test.go — what the dedup high-water mark does to a count message
// that arrives late, out of order, requeued or after an Edge restore, and what
// one delta costs Core in statements.
//
// The dedup UPSERT (claimDeltaSequence) advances last_seq only when the new seq
// is higher, so every message at or below the high-water mark is refused the
// same way: no ledger row, no exception, ErrInventoryDeltaSkipped. These pins
// record that at the base, so the change that gives each message its scope's
// running net has something exact to invert.

func netTestService(db *store.DB) *uop.InventoryDeltaService {
	return uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, service.EpochAnnounce{}), service.EpochAnnounce{})
}

func binUOP(t *testing.T, db *store.DB, binID int64) int {
	t.Helper()
	var got int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, binID).Scan(&got), "read uop_remaining")
	return got
}

func deltaLedgerRows(t *testing.T, db *store.DB, binID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger
		WHERE bin_id=$1 AND op='bin_uop_delta'`, binID).Scan(&n), "count delta ledger rows")
	return n
}

func exceptionRows(t *testing.T, db *store.DB, binID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_exception WHERE bin_id=$1`, binID).Scan(&n),
		"count exception rows")
	return n
}

func binEpoch(t *testing.T, db *store.DB, binID int64) int64 {
	t.Helper()
	var e int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, binID).Scan(&e), "read delta_epoch")
	return e
}

// seqDelta builds one bin delta with an explicit window, so a test can say
// which message is newer by Edge time as well as by seq.
func seqDelta(binID int64, delta int, seq, epoch int64, windowEnd time.Time) *protocol.BinUOPDelta {
	return &protocol.BinUOPDelta{
		BinID:       binID,
		PayloadCode: "PART-A",
		Delta:       delta,
		Reason:      protocol.ReasonConsumeTick,
		SequenceID:  seq,
		Epoch:       epoch,
		WindowStart: windowEnd.Add(-5 * time.Second),
		WindowEnd:   windowEnd,
	}
}

// P0a, Core half. A bin_uop_delta that dead-lettered on the Edge and is
// requeued after a later seq of its scope has applied is skipped silently.
//
// Verify-red: the running-net change (SYNTH-round2 §3, S3) inverts the COUNT
// half: seq 3 carries the scope's net, so seq 2's parts land with it and the
// requeue is a harmless skip. The skip itself (no row, no exception) stays.
func TestRunningNet_P0a_RequeueAfterLaterSeqIsSkippedSilently(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-P0A", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -2, 1, 0, t0)), "seq 1")
	// seq 2 (-4) dead-letters on the Edge; seq 3 goes through.
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -3, 3, 0, t0.Add(10*time.Second))), "seq 3")
	rowsBefore, excBefore := deltaLedgerRows(t, db, bin.ID), exceptionRows(t, db, bin.ID)

	// The hand requeue.
	err := svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -4, 2, 0, t0.Add(5*time.Second)))
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("requeued seq 2: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if got := binUOP(t, db, bin.ID); got != 95 {
		t.Errorf("uop_remaining = %d, want 95 (100-2-3: seq 2's 4 parts are lost at the base)", got)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != rowsBefore {
		t.Errorf("delta ledger rows = %d, want %d (the skip writes no row)", got, rowsBefore)
	}
	if got := exceptionRows(t, db, bin.ID); got != excBefore {
		t.Errorf("exception rows = %d, want %d (the skip writes no exception)", got, excBefore)
	}
}

// P0b. Seq 2 then seq 1 for one (station, bin, epoch) scope: seq 1 is muted
// and writes no ledger row.
//
// Verify-red: the running-net change (S3) inverts it. Seq 2 carries the net of
// both, so the total is applied and seq 1's late arrival is a harmless skip.
func TestRunningNet_P0b_ReorderMutesTheOlderMessage(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-P0B", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -3, 2, 0, t0.Add(5*time.Second))), "seq 2")
	err := svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -5, 1, 0, t0))
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("seq 1 after seq 2: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if got := binUOP(t, db, bin.ID); got != 97 {
		t.Errorf("uop_remaining = %d, want 97 (only seq 2's -3 lands at the base)", got)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != 1 {
		t.Errorf("delta ledger rows = %d, want 1 (the muted seq 1 writes none)", got)
	}
}

// P0d. Restore: the Edge's seq table rolled back, so seqs 6-10 are re-sent with
// NEW content (later windows) under last_seq = 10. Every one is skipped
// silently: no ledger row, no exception, the generation unchanged.
//
// Verify-red: the rollback rule (SYNTH-round2 S6) inverts it. The first re-sent
// message is detected (seq <= last_seq and window_end > applied_window_end),
// writes an edge_rollback exception and bumps the bin's generation; the rest
// are then stale-epoch drops.
func TestRunningNet_P0d_RestoreReplayIsSkippedSilently(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-P0D", "PART-A", 100)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=1 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "set epoch 1")
	t0 := time.Now().UTC().Add(-time.Hour)

	for seq := int64(1); seq <= 10; seq++ {
		testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
			seqDelta(bin.ID, -1, seq, 1, t0.Add(time.Duration(seq)*5*time.Second))), "original seq")
	}
	rowsBefore, excBefore := deltaLedgerRows(t, db, bin.ID), exceptionRows(t, db, bin.ID)

	// The restored Edge re-numbers from 6 with counts it took after the backup.
	later := time.Now().UTC()
	for seq := int64(6); seq <= 10; seq++ {
		err := svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -2, seq, 1, later.Add(time.Duration(seq)*time.Second)))
		if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
			t.Fatalf("restored seq %d: err = %v, want ErrInventoryDeltaSkipped", seq, err)
		}
	}
	if got := binUOP(t, db, bin.ID); got != 90 {
		t.Errorf("uop_remaining = %d, want 90 (the restored Edge's 10 parts never land)", got)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != rowsBefore {
		t.Errorf("delta ledger rows = %d, want %d", got, rowsBefore)
	}
	if got := exceptionRows(t, db, bin.ID); got != excBefore {
		t.Errorf("exception rows = %d, want %d (nothing records the rollback at the base)", got, excBefore)
	}
	if got := binEpoch(t, db, bin.ID); got != 1 {
		t.Errorf("delta_epoch = %d, want 1 (no bump at the base)", got)
	}
}

// P0g, Core half. Core keys a bucket scope by core_node_name. Two Edge node ids
// that share one core_node_name each number their own seq stream (the Edge keys
// by nodeID), and both land on ONE Core high-water row: the lower stream is
// muted.
//
// This half stays green: Core's key is the one both sides converge on. The
// verify-red is the Edge half (shingo-edge/uop running_net_test.go), which the
// one-bucket-key change (S4) inverts so the two streams are one.
func TestRunningNet_P0g_TwoEdgeStreamsOnOneCoreBucketKey(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	nodeName := sd.StorageNode.Name

	// Stream A (Edge node 34) has flushed five times.
	for seq := int64(1); seq <= 5; seq++ {
		testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation,
			makeBucketDelta(nodeName, "L1|U1", 100, "PART-G", 10, seq, protocol.ReasonCaptureFill)), "stream A")
	}
	// Stream B (Edge node 45, same core name) starts at seq 1.
	for seq := int64(1); seq <= 3; seq++ {
		err := svc.ApplyLinesideBucketDelta(testStation,
			makeBucketDelta(nodeName, "L1|U1", 100, "PART-G", 7, seq, protocol.ReasonCaptureFill))
		if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
			t.Fatalf("stream B seq %d: err = %v, want ErrInventoryDeltaSkipped", seq, err)
		}
	}
	var qty int
	testutil.MustNoErr(t, db.QueryRow(`SELECT qty FROM lineside_buckets
		WHERE core_node_name=$1 AND pair_key='L1|U1' AND style_id=100 AND payload_code='PART-G'`, nodeName).Scan(&qty),
		"read bucket")
	if qty != 50 {
		t.Errorf("bucket qty = %d, want 50 (stream B's 21 parts are muted)", qty)
	}
}

// Core statements per bin delta, and per bucket delta, on the steady-state
// path (the dedup row exists; a consume tick on a bin at a node, a capture fill
// on a bucket), counted at pgx's tracer.
//
// THE COUNT INCLUDES BEGIN AND COMMIT (or the ROLLBACK a skip ends in). pgx's
// stdlib BeginTx sends "begin" through the traced Exec, so store/query_count.go's
// "not ... the BEGIN/COMMIT of a Tx" does not hold for this path; measured here.
//
// At the base: a bin delta is 7 (begin, the epoch read, the dedup UPSERT, the
// bin read, the UPDATE, the ledger INSERT, commit); a skipped duplicate is 4
// (begin, epoch read, UPSERT, rollback); a bucket delta is 6 (the node lookup
// outside the tx, begin, the dedup UPSERT, the bucket UPSERT, the GC DELETE,
// commit). The running-net change must not move any of them (SYNTH-round2 §3:
// the prior dedup read folds into the dedup statement).
func TestRunningNet_CoreStatementsPerDelta(t *testing.T) {
	t.Parallel()
	_, cfg := testdb.OpenWithConfig(t)
	cdb, counter, err := store.OpenCounting(cfg)
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	sd := testdb.SetupStandardData(t, cdb)
	svc := netTestService(cdb)
	bin := createTestBin(t, cdb, sd.StorageNode.ID, "BIN-NET-COUNT", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -1, 1, 0, t0)), "seq 1")
	counter.Reset()
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -1, 2, 0, t0.Add(5*time.Second))), "seq 2")
	if got := counter.Count(); got != wantBinDeltaStatements {
		t.Errorf("bin delta: %d statements, want %d", got, wantBinDeltaStatements)
	}

	counter.Reset()
	err = svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -1, 2, 0, t0.Add(5*time.Second)))
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("duplicate: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if got := counter.Count(); got != wantBinSkipStatements {
		t.Errorf("bin duplicate: %d statements, want %d", got, wantBinSkipStatements)
	}

	nodeName := sd.StorageNode.Name
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation,
		makeBucketDelta(nodeName, "L1|U1", 100, "PART-C", 10, 1, protocol.ReasonCaptureFill)), "bucket seq 1")
	counter.Reset()
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation,
		makeBucketDelta(nodeName, "L1|U1", 100, "PART-C", 5, 2, protocol.ReasonCaptureFill)), "bucket seq 2")
	if got := counter.Count(); got != wantBucketDeltaStatements {
		t.Errorf("bucket delta: %d statements, want %d", got, wantBucketDeltaStatements)
	}
}

// The measured budgets. See TestRunningNet_CoreStatementsPerDelta.
const (
	wantBinDeltaStatements    = 7
	wantBinSkipStatements     = 4
	wantBucketDeltaStatements = 6
)
