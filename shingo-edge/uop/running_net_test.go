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
	r := New(db, "stn-test", nil, nil, nil)

	r.RecordBin(42, "PART-A", -6, protocol.ReasonConsumeTick, 1)
	r.Flush()
	r.RecordBin(42, "PART-A", -4, protocol.ReasonConsumeTick, 1)
	r.Flush()
	r.RecordBucket(5, "CORE-NODE-1", "L1|U1", 100, "PART-B", 47, protocol.ReasonCaptureFill)
	r.Flush()
	r.RecordBucket(5, "CORE-NODE-1", "L1|U1", 100, "PART-B", -5, protocol.ReasonConsumeDrain)
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
	buckets := pendingOutboxByType[protocol.LinesideBucketDelta](t, db, protocol.SubjectLinesideBucketDelta)
	if len(buckets) != 2 {
		t.Fatalf("queued %d bucket envelopes, want 2", len(buckets))
	}
	for i, want := range []int64{47, 42} {
		if buckets[i].Net == nil || *buckets[i].Net != want {
			t.Errorf("bucket message %d: Net = %v, want %d", i, buckets[i].Net, want)
		}
	}
}

// A flush whose seq UPSERT succeeds and whose outbox INSERT fails has already
// added the delta to the scope's net. The entry keeps the delta for the next
// flush, which must add only what is new, or the net would carry the failed
// window twice and Core would apply it twice.
func TestRunningNet_FailedEnqueueDoesNotDoubleTheNet(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "stn-test", nil, nil, nil)

	r.RecordBin(42, "PART-A", -6, protocol.ReasonConsumeTick, 1)
	_, err := db.Exec(`ALTER TABLE outbox RENAME TO outbox_away`)
	testutil.MustNoErr(t, err, "take the outbox away")
	r.Flush() // allocates seq 1 (net -6), then the INSERT fails
	_, err = db.Exec(`ALTER TABLE outbox_away RENAME TO outbox`)
	testutil.MustNoErr(t, err, "put the outbox back")

	r.RecordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 1)
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
// in memory until the flush), a flush issues 2 per dirty scope (the seq UPSERT
// and the outbox INSERT), and a flush with nothing dirty issues 0. The running
// net must leave all three unchanged.
func TestRunningNet_AccumulatorStatementsPerTickAndFlush(t *testing.T) {
	t.Parallel()
	db, counter := openCountingAccumulatorDB(t)
	r := New(db, "stn-test", nil, nil, nil)

	counter.Reset()
	for i := 0; i < 5; i++ {
		r.RecordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 1)
		r.RecordBucket(5, "CORE-NODE-1", "L1|U1", 100, "PART-B", -1, protocol.ReasonConsumeDrain)
	}
	if got := counter.Count(); got != 0 {
		t.Errorf("10 recorded ticks: %d statements, want 0", got)
	}

	counter.Reset()
	r.Flush()
	if got := counter.Count(); got != 4 {
		t.Errorf("flush of one bin and one bucket: %d statements, want 4 (2 per dirty scope)", got)
	}

	counter.Reset()
	r.Flush()
	if got := counter.Count(); got != 0 {
		t.Errorf("flush with nothing dirty: %d statements, want 0", got)
	}
}

// P0g, Edge half, inverted. Two Edge process nodes that carry ONE
// core_node_name used to number their bucket deltas in two separate seq
// streams (the Edge keyed a bucket scope by its local nodeID, Core by
// core_node_name), so both went out as seq 1 and Core muted the second.
//
// The one-bucket-key change (SYNTH-round2 S4) keys the Edge's seq scope by
// core_node_name, so the two nodes share one stream: seq 1, then seq 2.
func TestRunningNet_P0g_TwoNodesOneCoreNameShareOneStream(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "stn-test", nil, nil, nil)

	r.RecordBucket(34, "SMN-TEST", "L1|U1", 100, "PART-G", 10, protocol.ReasonCaptureFill)
	r.Flush()
	r.RecordBucket(45, "SMN-TEST", "L1|U1", 100, "PART-G", 7, protocol.ReasonCaptureFill)
	r.Flush()

	deltas := pendingOutboxByType[protocol.LinesideBucketDelta](t, db, protocol.SubjectLinesideBucketDelta)
	if len(deltas) != 2 {
		t.Fatalf("queued %d bucket envelopes, want 2", len(deltas))
	}
	if deltas[0].CoreNodeName != deltas[1].CoreNodeName {
		t.Fatalf("core names %q / %q, want one", deltas[0].CoreNodeName, deltas[1].CoreNodeName)
	}
	if deltas[0].SequenceID != 1 || deltas[1].SequenceID != 2 {
		t.Errorf("seqs = %d, %d, want 1, 2 (one stream per Core scope)",
			deltas[0].SequenceID, deltas[1].SequenceID)
	}
}
