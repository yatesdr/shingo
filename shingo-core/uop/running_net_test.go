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

// P0a, Core half, inverted. A bin_uop_delta that dead-lettered on the Edge and
// is requeued after a later seq of its scope has applied is still skipped with
// no ledger row and no exception, but its parts are no longer lost: seq 3
// carried the scope's net, which already held seq 2's 4 parts (SYNTH-round2
// §3, S3). At the base the count read 95.
func TestRunningNet_P0a_RequeueAfterLaterSeqIsHarmless(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-P0A", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -2, -2, 1, 0, t0)), "seq 1")
	// seq 2 (-4, net -6) dead-letters on the Edge; seq 3 goes through.
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -3, -9, 3, 0, t0.Add(10*time.Second))), "seq 3")
	rowsBefore, excBefore := deltaLedgerRows(t, db, bin.ID), exceptionRows(t, db, bin.ID)

	// The hand requeue.
	err := svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -4, -6, 2, 0, t0.Add(5*time.Second)))
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("requeued seq 2: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if got := binUOP(t, db, bin.ID); got != 91 {
		t.Errorf("uop_remaining = %d, want 91 (100-2-3-4: seq 3's net carried seq 2)", got)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != rowsBefore {
		t.Errorf("delta ledger rows = %d, want %d (the skip writes no row)", got, rowsBefore)
	}
	if got := exceptionRows(t, db, bin.ID); got != excBefore {
		t.Errorf("exception rows = %d, want %d (the skip writes no exception)", got, excBefore)
	}
}

// P0b, inverted. Seq 2 then seq 1 for one (station, bin, epoch) scope. Seq 2
// carries the net of both, so the total lands on its arrival and seq 1 is then
// a harmless skip (S3). At the base only seq 2's -3 landed.
//
// The one ledger row says what happened: delta is what was applied (-8),
// wire_delta what seq 2 said (-3), healed the difference (-5).
func TestRunningNet_P0b_ReorderAppliesTheTotal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := netTestService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-NET-P0B", "PART-A", 100)
	t0 := time.Now().UTC()

	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -3, -8, 2, 0, t0.Add(5*time.Second))), "seq 2")
	err := svc.ApplyBinUOPDelta(testStation, netDelta(bin.ID, -5, -5, 1, 0, t0))
	if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("seq 1 after seq 2: err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if got := binUOP(t, db, bin.ID); got != 92 {
		t.Errorf("uop_remaining = %d, want 92 (the total, -8)", got)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != 1 {
		t.Errorf("delta ledger rows = %d, want 1", got)
	}
	m := lastDeltaMeta(t, db, bin.ID)
	if m.Delta != -8 || m.WireDelta != -3 || m.Healed != -5 || m.Net == nil || *m.Net != -8 || m.SequenceID != 2 {
		t.Errorf("metadata = %+v, want delta -8, wire_delta -3, healed -5, net -8, sequence_id 2", m)
	}
	if m.WindowStart == "" || m.WindowEnd == "" {
		t.Errorf("metadata = %+v, want window_start and window_end", m)
	}
	var before, after int
	testutil.MustNoErr(t, db.QueryRow(`SELECT before_uop, after_uop FROM bin_uop_ledger
		WHERE bin_id=$1 AND op='bin_uop_delta'`, bin.ID).Scan(&before, &after), "read before/after")
	if before != 100 || after != 92 {
		t.Errorf("before/after = %d/%d, want 100/92 (the applied delta, not the wire one)", before, after)
	}
}

// P0d, inverted. Restore: the Edge's seq table rolled back, so seqs 6-10 are
// re-sent with NEW content (later windows) under last_seq = 10.
//
// At the base every one was skipped silently. Under the rollback rule
// (SYNTH-round2 S6) the first re-sent message is detected — seq <= last_seq AND
// window_end > applied_window_end — and writes an edge_rollback exception, then
// bumps the bin's generation so the Edge adopts Core's number under a fresh
// scope. The rest carry the retired generation and are stale-epoch drops. The
// restored Edge's counts are not applied: its seq table and its net both went
// backward, so nothing it re-sends in the old generation can be trusted.
func TestRunningNet_P0d_RestoreIsDetectedAndBumps(t *testing.T) {
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
	rowsBefore := deltaLedgerRows(t, db, bin.ID)

	// The restored Edge re-numbers from 6 with counts it took after the backup.
	later := time.Now().UTC()
	for seq := int64(6); seq <= 10; seq++ {
		err := svc.ApplyBinUOPDelta(testStation, seqDelta(bin.ID, -2, seq, 1, later.Add(time.Duration(seq)*time.Second)))
		if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
			t.Fatalf("restored seq %d: err = %v, want ErrInventoryDeltaSkipped", seq, err)
		}
	}
	if got := binUOP(t, db, bin.ID); got != 90 {
		t.Errorf("uop_remaining = %d, want 90 (a rolled-back stream is not applied)", got)
	}
	if got := deltaLedgerRows(t, db, bin.ID); got != rowsBefore {
		t.Errorf("delta ledger rows = %d, want %d", got, rowsBefore)
	}
	var rollbacks int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_exception
		WHERE bin_id=$1 AND kind='edge_rollback'`, bin.ID).Scan(&rollbacks), "count rollback exceptions")
	if rollbacks != 1 {
		t.Errorf("edge_rollback exception rows = %d, want 1 (detected once; the rest are stale-epoch drops)", rollbacks)
	}
	if got := binEpoch(t, db, bin.ID); got != 2 {
		t.Errorf("delta_epoch = %d, want 2 (the rollback bumps the generation)", got)
	}
	var stale int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger
		WHERE bin_id=$1 AND op='stale_epoch_dropped'`, bin.ID).Scan(&stale), "count stale drops")
	if stale != 4 {
		t.Errorf("stale_epoch_dropped rows = %d, want 4 (seqs 7-10 after the bump)", stale)
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
// commit). The running-net change must not raise any of them (SYNTH-round2 §3:
// the prior dedup read folds into an existing statement). It lowers one: the
// cursor rides the epoch read, so a skipped duplicate is decided there, before
// the UPSERT, and costs 3 (begin, the read, rollback).
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
	t0 := time.Now().UTC()

	// Both wire shapes: an old Edge's (no net) and a new one's.
	for _, withNet := range []bool{false, true} {
		label := "BIN-NET-COUNT-OLD"
		part := "PART-C"
		if withNet {
			label, part = "BIN-NET-COUNT-NEW", "PART-D"
		}
		bin := createTestBin(t, cdb, sd.StorageNode.ID, label, "PART-A", 100)
		mk := func(seq int64) *protocol.BinUOPDelta {
			d := seqDelta(bin.ID, -1, seq, 0, t0.Add(time.Duration(seq)*5*time.Second))
			if withNet {
				d.Net = int64p(-seq)
			}
			return d
		}
		testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, mk(1)), "seq 1")
		counter.Reset()
		testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, mk(2)), "seq 2")
		if got := counter.Count(); got != wantBinDeltaStatements {
			t.Errorf("net=%v bin delta: %d statements, want %d", withNet, got, wantBinDeltaStatements)
		}

		counter.Reset()
		err = svc.ApplyBinUOPDelta(testStation, mk(2))
		if !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
			t.Fatalf("net=%v duplicate: err = %v, want ErrInventoryDeltaSkipped", withNet, err)
		}
		if got := counter.Count(); got != wantBinSkipStatements {
			t.Errorf("net=%v bin duplicate: %d statements, want %d", withNet, got, wantBinSkipStatements)
		}

		nodeName := sd.StorageNode.Name
		mkb := func(seq int64) *protocol.LinesideBucketDelta {
			d := makeBucketDelta(nodeName, "L1|U1", 100, part, 5, seq, protocol.ReasonCaptureFill)
			if withNet {
				d.Net = int64p(5 * seq)
			}
			return d
		}
		testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation, mkb(1)), "bucket seq 1")
		counter.Reset()
		testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta(testStation, mkb(2)), "bucket seq 2")
		if got := counter.Count(); got != wantBucketDeltaStatements {
			t.Errorf("net=%v bucket delta: %d statements, want %d", withNet, got, wantBucketDeltaStatements)
		}
	}
}

// The measured budgets. See TestRunningNet_CoreStatementsPerDelta.
const (
	wantBinDeltaStatements    = 7
	wantBinSkipStatements     = 3
	wantBucketDeltaStatements = 6
)
