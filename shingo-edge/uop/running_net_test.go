package uop

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// running_net_test.go — the Edge half of the count-message pins: what one seq
// allocation and one flush cost the Pi in statements, and how the accumulator
// numbers a bucket scope.

func openCountingAccumulatorDB(t *testing.T) (*store.DB, *store.QueryCounter) {
	t.Helper()
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "net.db"))
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, counter
}

// AllocateInventoryDeltaSeq is ONE statement per call: an UPSERT ... RETURNING.
// The running net (SYNTH-round2 §3, V12) rides that same statement: it adds the
// flushed delta to the scope's net and returns both, still 1 statement.
func TestRunningNet_AllocateSeqIsOneStatement(t *testing.T) {
	t.Parallel()
	db, counter := openCountingAccumulatorDB(t)

	for _, tc := range []struct {
		add, wantSeq, wantNet int64
	}{{-3, 1, -3}, {-2, 2, -5}, {7, 3, 2}} {
		counter.Reset()
		seq, net, err := db.AllocateInventoryDeltaSeq(invDeltaScopeBin, "42", 1, tc.add)
		testutil.MustNoErr(t, err, "allocate")
		if seq != tc.wantSeq || net != tc.wantNet {
			t.Errorf("add %d: seq/net = %d/%d, want %d/%d", tc.add, seq, net, tc.wantSeq, tc.wantNet)
		}
		if got := counter.Count(); got != 1 {
			t.Errorf("allocate seq %d: %d statements, want 1", tc.wantSeq, got)
		}
	}
	// A new epoch is a new scope: its seq and its net both start over.
	seq, net, err := db.AllocateInventoryDeltaSeq(invDeltaScopeBin, "42", 2, -1)
	testutil.MustNoErr(t, err, "allocate epoch 2")
	if seq != 1 || net != -1 {
		t.Errorf("epoch 2: seq/net = %d/%d, want 1/-1", seq, net)
	}
}

// The flush stamps each message with its scope's running net: the signed sum
// of every delta this Edge has flushed for the scope, this one included.
func TestRunningNet_FlushStampsTheRunningNet(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "stn-test", nil, nil)

	r.acc.recordBin(42, "PART-A", -6, protocol.ReasonConsumeTick, 1)
	r.Flush()
	r.acc.recordBin(42, "PART-A", -4, protocol.ReasonConsumeTick, 1)
	r.Flush()

	bins := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(bins) != 2 {
		t.Fatalf("queued %d bin envelopes, want 2", len(bins))
	}
	for i, want := range []int64{-6, -10} {
		if bins[i].Net == nil || *bins[i].Net != want {
			t.Errorf("bin message %d: Net = %v, want %d", i, bins[i].Net, want)
		}
	}
	// The bucket half went under change #1: a pile level carries its whole
	// row, so there is no net to stamp (bucket_level_test.go pins the level).
}

// A flush whose seq UPSERT succeeds and whose outbox INSERT fails has already
// added the delta to the scope's net. The entry keeps the delta for the next
// flush, which must add only what is new, or the net would carry the failed
// window twice and Core would apply it twice.
func TestRunningNet_FailedEnqueueDoesNotDoubleTheNet(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "stn-test", nil, nil)

	r.acc.recordBin(42, "PART-A", -6, protocol.ReasonConsumeTick, 1)
	_, err := db.Exec(`ALTER TABLE outbox RENAME TO outbox_away`)
	testutil.MustNoErr(t, err, "take the outbox away")
	r.Flush() // allocates seq 1 (net -6), then the INSERT fails
	_, err = db.Exec(`ALTER TABLE outbox_away RENAME TO outbox`)
	testutil.MustNoErr(t, err, "put the outbox back")

	r.acc.recordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 1)
	r.Flush()

	bins := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(bins) != 1 {
		t.Fatalf("queued %d bin envelopes, want 1", len(bins))
	}
	got := bins[0]
	if got.Delta != -7 || got.SequenceID != 2 || got.Net == nil || *got.Net != -7 {
		t.Errorf("message = delta %d seq %d net %v, want delta -7, seq 2 (seq 1 burned), net -7",
			got.Delta, got.SequenceID, got.Net)
	}
}

// The accumulator's cost on the Pi: a recorded tick issues 0 statements (it is
// in memory until the flush), a flush issues 2 per dirty bin scope (the seq
// UPSERT and the outbox INSERT) and 3 per dirty pile key (the level read, the
// seq UPSERT and the outbox INSERT), and a flush with nothing dirty issues 0.
// Changed under change #1: a dirty pile costs the one level read the lead's
// read-at-flush decision adds (it was 2, carrying a delta).
func TestRunningNet_AccumulatorStatementsPerTickAndFlush(t *testing.T) {
	t.Parallel()
	db, counter := openCountingAccumulatorDB(t)
	r := New(db, "stn-test", nil, nil)

	counter.Reset()
	for i := 0; i < 5; i++ {
		r.acc.recordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 1)
		r.acc.markBucket(5, "CORE-NODE-1", "PART-B", protocol.LinesideBucketActive, 1)
	}
	if got := counter.Count(); got != 0 {
		t.Errorf("10 recorded ticks: %d statements, want 0", got)
	}

	counter.Reset()
	r.Flush()
	if got := counter.Count(); got != 5 {
		t.Errorf("flush of one bin and one pile: %d statements, want 5 (2 for the bin, 3 for the pile)", got)
	}

	counter.Reset()
	r.Flush()
	if got := counter.Count(); got != 0 {
		t.Errorf("flush with nothing dirty: %d statements, want 0", got)
	}
}

// P0g, Edge half. Two Edge process nodes that carry ONE core_node_name used to
// number their bucket deltas in two separate seq streams, so both went out as
// seq 1 and Core muted the second. A pile level is keyed by core_node_name, so
// the two nodes share one stream: seq 1, then seq 2 (the level they carry, the
// sum over both nodes, is pinned in bucket_level_test.go).
// Changed under change #1: the messages are levels, keyed without pair or
// style.
func TestRunningNet_P0g_TwoNodesOneCoreNameShareOneStream(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "stn-test", nil, nil)

	r.acc.markBucket(34, "SMN-TEST", "PART-G", protocol.LinesideBucketActive, 0)
	r.Flush()
	r.acc.markBucket(45, "SMN-TEST", "PART-G", protocol.LinesideBucketActive, 0)
	r.Flush()

	levels := pendingOutboxByType[protocol.LinesideBucketLevel](t, db, protocol.SubjectLinesideBucketLevel)
	if len(levels) != 2 {
		t.Fatalf("queued %d level envelopes, want 2", len(levels))
	}
	if levels[0].CoreNodeName != levels[1].CoreNodeName {
		t.Fatalf("core names %q / %q, want one", levels[0].CoreNodeName, levels[1].CoreNodeName)
	}
	if levels[0].SequenceID != 1 || levels[1].SequenceID != 2 {
		t.Errorf("seqs = %d, %d, want 1, 2 (one stream per Core scope)",
			levels[0].SequenceID, levels[1].SequenceID)
	}
}
