package uop

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

func newReporterTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "rep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// pendingOutboxByType returns enqueued outbox payloads matching the
// given subject (which is also the MsgType the reporter writes to the
// outbox). Each row carries a protocol.Envelope with a Data payload
// that wraps the actual subject body — this helper unwraps both
// layers so tests assert on the typed payload directly.
func pendingOutboxByType[T any](t *testing.T, db *store.DB, subject string) []T {
	t.Helper()
	msgs, err := db.ListPendingOutbox(1000)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	var out []T
	for _, m := range msgs {
		if m.MsgType != subject {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode data")
		var p T
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &p), "decode payload")
		out = append(out, p)
	}
	return out
}

// TestInventoryDeltaReporter_BinAccumulationAndFlush pins the bin path:
// multiple RecordBin calls in the same window aggregate into a single
// envelope with the summed delta and a monotonic SequenceID.
func TestInventoryDeltaReporter_BinAccumulationAndFlush(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil, nil)

	r.RecordBin(42, "PART-A", -3, protocol.ReasonConsumeTick)
	r.RecordBin(42, "PART-A", -2, protocol.ReasonConsumeTick)
	r.RecordBin(42, "PART-A", -1, protocol.ReasonConsumeTick)
	r.Flush()

	deltas := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(deltas) != 1 {
		t.Fatalf("queued %d BinUOPDelta envelopes, want 1 (deltas should aggregate)", len(deltas))
	}
	got := deltas[0]
	if got.BinID != 42 {
		t.Errorf("BinID = %d, want 42", got.BinID)
	}
	if got.Delta != -6 {
		t.Errorf("Delta = %d, want -6 (sum of -3, -2, -1)", got.Delta)
	}
	if got.PayloadCode != "PART-A" {
		t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, "PART-A")
	}
	if got.Reason != protocol.ReasonConsumeTick {
		t.Errorf("Reason = %q, want %q", got.Reason, protocol.ReasonConsumeTick)
	}
	if got.SequenceID != 1 {
		t.Errorf("SequenceID = %d, want 1 (first flush)", got.SequenceID)
	}
	if got.Station != "ALN_001" {
		t.Errorf("Station = %q, want %q", got.Station, "ALN_001")
	}
}

// TestInventoryDeltaReporter_BucketAccumulation pins the bucket path
// mirror: per-(node, pair_key, style, part) keying, capture_fill +
// consume_drain accumulate, sequence advances monotonically.
func TestInventoryDeltaReporter_BucketAccumulation(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "SMN_003", nil, nil, nil)

	r.RecordBucket(5, "CORE-LOADER-1", "L1|U1", 100, "PART-A", "WIDGET-A", 47, protocol.ReasonCaptureFill)
	r.Flush()
	r.RecordBucket(5, "CORE-LOADER-1", "L1|U1", 100, "PART-A", "WIDGET-A", -3, protocol.ReasonConsumeDrain)
	r.RecordBucket(5, "CORE-LOADER-1", "L1|U1", 100, "PART-A", "WIDGET-A", -2, protocol.ReasonConsumeDrain)
	r.Flush()

	deltas := pendingOutboxByType[protocol.LinesideBucketDelta](t, db, protocol.SubjectLinesideBucketDelta)
	if len(deltas) != 2 {
		t.Fatalf("queued %d bucket envelopes, want 2 (one per flush)", len(deltas))
	}
	if deltas[0].Delta != 47 || deltas[0].Reason != protocol.ReasonCaptureFill {
		t.Errorf("first envelope: delta=%d reason=%q, want 47 / capture_fill",
			deltas[0].Delta, deltas[0].Reason)
	}
	if deltas[1].Delta != -5 || deltas[1].Reason != protocol.ReasonConsumeDrain {
		t.Errorf("second envelope: delta=%d reason=%q, want -5 / consume_drain",
			deltas[1].Delta, deltas[1].Reason)
	}
	if deltas[0].SequenceID != 1 || deltas[1].SequenceID != 2 {
		t.Errorf("seq sequence = %d, %d, want 1, 2", deltas[0].SequenceID, deltas[1].SequenceID)
	}
	if deltas[0].PartNumber != "PART-A" {
		t.Errorf("PartNumber = %q, want PART-A", deltas[0].PartNumber)
	}
	// Round-3 Obs 8: wire envelope must carry the cross-system identifier,
	// not Edge's local process_nodes.id.
	if deltas[0].CoreNodeName != "CORE-LOADER-1" {
		t.Errorf("CoreNodeName = %q, want CORE-LOADER-1 (Obs 8 — node ID namespace fix)", deltas[0].CoreNodeName)
	}
}

// TestInventoryDeltaReporter_ZeroDeltaNoFlush pins a no-op invariant:
// when net delta in a window cancels to zero (record +5 then -5), the
// flush emits nothing and the SequenceID does not advance — gaps are
// fine, but burning seq numbers on no-op flushes wastes audit space.
//
// Implementation note: today the reporter checks delta==0 before
// allocating a seq, so a +5/-5 pair flushes nothing. A future change
// that always allocates would fail this test before it ships.
func TestInventoryDeltaReporter_ZeroDeltaNoFlush(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil, nil)

	r.RecordBin(7, "PART-X", 5, protocol.ReasonProduceTick)
	r.RecordBin(7, "PART-X", -5, protocol.ReasonConsumeTick)
	r.Flush()

	deltas := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(deltas) != 0 {
		t.Errorf("queued %d envelopes for cancelled-out window, want 0", len(deltas))
	}

	// Sequence should NOT have advanced — no envelope went out.
	var seq int64
	_ = db.QueryRow(`SELECT next_seq FROM inventory_delta_seq WHERE scope_kind='bin' AND scope_key=?`,
		strconv.Itoa(7)).Scan(&seq)
	if seq != 0 {
		t.Errorf("next_seq = %d, want 0 (no envelope emitted)", seq)
	}
}

// TestInventoryDeltaReporter_ScopeKeysIndependent pins that two
// distinct scopes (different bin IDs, or different bucket composite
// keys) accumulate and flush independently. Each gets its own
// SequenceID stream.
func TestInventoryDeltaReporter_ScopeKeysIndependent(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil, nil)

	r.RecordBin(11, "PART-A", -1, protocol.ReasonConsumeTick)
	r.RecordBin(22, "PART-B", -2, protocol.ReasonConsumeTick)
	r.RecordBin(33, "PART-C", -3, protocol.ReasonConsumeTick)
	r.Flush()

	deltas := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(deltas) != 3 {
		t.Fatalf("queued %d envelopes, want 3 (one per bin)", len(deltas))
	}
	byID := map[int64]protocol.BinUOPDelta{}
	for _, d := range deltas {
		byID[d.BinID] = d
	}
	if byID[11].Delta != -1 || byID[22].Delta != -2 || byID[33].Delta != -3 {
		t.Errorf("per-bin deltas: got 11=%d 22=%d 33=%d; want -1, -2, -3",
			byID[11].Delta, byID[22].Delta, byID[33].Delta)
	}
	// Each scope starts its own seq stream at 1.
	for binID, d := range byID {
		if d.SequenceID != 1 {
			t.Errorf("bin %d SequenceID = %d, want 1 (independent scope)",
				binID, d.SequenceID)
		}
	}
}

// TestInventoryDeltaReporter_RestoreOnEnqueueFailure pins the
// restore-on-failure invariant: when the outbox enqueue fails, the
// swept delta folds back into the live entry so a subsequent
// successful flush still ships the count change. Mirror of
// production_reporter's restoreSnapshot pattern.
//
// We force failure by closing the underlying DB before flush.
func TestInventoryDeltaReporter_RestoreOnEnqueueFailure(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil, nil)

	r.RecordBin(99, "PART-Z", -7, protocol.ReasonConsumeTick)

	// Close the DB so EnqueueOutbox / AllocateInventoryDeltaSeq fail.
	db.Close()
	r.Flush()

	// The entry must still hold -7.
	v, ok := r.acc.bins.Load("99")
	if !ok {
		t.Fatal("bin entry disappeared after failed flush — must be restored")
	}
	e := v.(*binDeltaEntry)
	e.mu.Lock()
	got := e.delta
	e.mu.Unlock()
	if got != -7 {
		t.Errorf("delta after failed flush = %d, want -7 (must be restored)", got)
	}
}

// TestInventoryDeltaReporter_ConcurrentRecordsThenFlush pins
// thread-safety of the sync.Map + per-entry mutex design under
// contended writes. 100 goroutines × 10 deltas to the same bin must
// sum to a deterministic 1000 on flush, with no lost updates and no
// data races (run with -race in CI).
func TestInventoryDeltaReporter_ConcurrentRecordsThenFlush(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil, nil)

	const goroutines = 100
	const perGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				r.RecordBin(50, "PART-CONC", 1, protocol.ReasonProduceTick)
			}
		}()
	}
	wg.Wait()
	r.Flush()

	deltas := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(deltas) != 1 {
		t.Fatalf("queued %d envelopes, want 1", len(deltas))
	}
	if deltas[0].Delta != goroutines*perGoroutine {
		t.Errorf("aggregated delta = %d, want %d (no lost updates)",
			deltas[0].Delta, goroutines*perGoroutine)
	}
}

// TestInventoryDeltaReporter_StopFlushesPending pins shutdown
// semantics: Stop runs one final flush so deltas accumulated up to
// the moment of shutdown still reach the outbox. Without this,
// graceful Edge shutdown after a release click would lose any
// in-flight delta.
func TestInventoryDeltaReporter_StopFlushesPending(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil, nil)
	r.SetInterval(1 * time.Hour) // never auto-flush
	r.Start()

	r.RecordBin(60, "PART-S", -4, protocol.ReasonConsumeTick)
	r.Stop()

	deltas := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(deltas) != 1 {
		t.Fatalf("queued %d envelopes after Stop, want 1 (Stop must flush pending)", len(deltas))
	}
	if deltas[0].Delta != -4 {
		t.Errorf("delta = %d, want -4", deltas[0].Delta)
	}
}

// TestInventoryDeltaReporter_BucketScopeKeyComposite pins the
// pipe-delimited composite scope key for buckets — this format
// matches the Core-side dedup key on inventory_delta_dedup. A drift
// between the two sides would make every bucket delta look like a
// new scope, defeating dedup.
func TestInventoryDeltaReporter_BucketScopeKeyComposite(t *testing.T) {
	t.Parallel()
	got := bucketScopeKey(5, "L1|U1", 100, "PART-A")
	want := "5|L1|U1|100|PART-A"
	if got != want {
		t.Errorf("bucketScopeKey = %q, want %q (Core's dedup expects this exact format)", got, want)
	}
}

// Pending-delta guard tests removed alongside the reconciler deletion
// (bin-ownership flip): with no reconciler healing, there is no caller
// for IsPendingBinDelta / IsPendingBucketDelta / LoadPendingFromOutbox.
// Outbox health is now signaled exclusively via FlushFailures + Kafka
// consumer-lag dashboards; correctness lives in inventory_delta_dedup
// at Core.
