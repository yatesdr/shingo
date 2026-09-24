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
// The running net (SYNTH-round2 §3, V12) must ride this same statement, so the
// count stays 1 after that change.
func TestRunningNet_AllocateSeqIsOneStatement(t *testing.T) {
	t.Parallel()
	db, counter := openCountingAccumulatorDB(t)

	for _, want := range []int64{1, 2} {
		counter.Reset()
		seq, err := db.AllocateInventoryDeltaSeq(invDeltaScopeBin, "42", 1)
		testutil.MustNoErr(t, err, "allocate")
		if seq != want {
			t.Errorf("seq = %d, want %d", seq, want)
		}
		if got := counter.Count(); got != 1 {
			t.Errorf("allocate #%d: %d statements, want 1", want, got)
		}
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

// P0g, Edge half. Two Edge process nodes that carry ONE core_node_name number
// their bucket deltas in two separate seq streams, because the Edge keys a
// bucket scope by its local nodeID while Core keys it by core_node_name. Both
// flushes go out as seq 1 for what Core sees as one scope, and Core mutes the
// second (the Core half of this pin).
//
// Verify-red: the one-bucket-key change (SYNTH-round2 S4) inverts it. The Edge
// keys by core_node_name, so the two nodes share one stream and the second
// message is seq 2.
func TestRunningNet_P0g_TwoNodesOneCoreNameNumberTwoStreams(t *testing.T) {
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
	if deltas[0].SequenceID != 1 || deltas[1].SequenceID != 1 {
		t.Errorf("seqs = %d, %d, want 1, 1 (two streams for one Core scope at the base)",
			deltas[0].SequenceID, deltas[1].SequenceID)
	}
}
