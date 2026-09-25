package uop

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/processes"
)

// capture_pins_test.go — the release-click capture, end to end through the real
// Mutator and the real schema: which pile rows the capture leaves, and what the
// accumulator puts on the wire for them (the bucket delta and the bin's
// capture_reduction). Pinned before the bucket-level change; each test names
// the expected change it flips under, or says it stays.

type captureFixture struct {
	db       *store.DB
	m        *Mutator
	nodeID   int64
	styleA   int64
	styleB   int64
	styleC   int64
	coreSeat string
}

func newCaptureFixture(t *testing.T, prefix string) *captureFixture {
	t.Helper()
	db := newReporterTestDB(t)
	procID, err := db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: prefix + "-SEAT", Code: "C1",
		Name: prefix + "-SEAT", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	f := &captureFixture{db: db, nodeID: nodeID, coreSeat: prefix + "-SEAT"}
	for _, s := range []struct {
		name string
		id   *int64
	}{{"-A", &f.styleA}, {"-B", &f.styleB}, {"-C", &f.styleC}} {
		id, err := db.CreateStyle(prefix+s.name, "", procID)
		testutil.MustNoErr(t, err, "create style")
		*s.id = id
	}
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &f.styleA), "set active style")
	f.m = New(db, "stn-test", db, db, db)
	return f
}

func (f *captureFixture) capture(t *testing.T, styleID int64, binID int64, suppressBin bool, qty map[string]int) int {
	t.Helper()
	n, err := f.m.CaptureToLineside(CaptureEvent{
		NodeID: f.nodeID, StyleID: styleID, CoreNodeName: f.coreSeat,
		Disposition: ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: qty},
		BinID:       binID, PayloadCode: "SYN-BIN-PART", BinEpoch: 1,
		SuppressBinDelta: suppressBin,
	})
	testutil.MustNoErr(t, err, "CaptureToLineside")
	return n
}

func (f *captureFixture) rows(t *testing.T) []lineside.Bucket {
	t.Helper()
	rows, err := f.db.ListLinesideBuckets(f.nodeID)
	testutil.MustNoErr(t, err, "list buckets")
	return rows
}

func (f *captureFixture) flushed(t *testing.T) ([]protocol.LinesideBucketDelta, []protocol.BinUOPDelta) {
	t.Helper()
	f.m.Flush()
	buckets := pendingOutboxByType[protocol.LinesideBucketDelta](t, f.db, protocol.SubjectLinesideBucketDelta)
	bins := pendingOutboxByType[protocol.BinUOPDelta](t, f.db, protocol.SubjectBinUOPDelta)
	msgs, err := f.db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	for _, msg := range msgs {
		testutil.MustNoErr(t, f.db.AckOutbox(msg.ID), "ack outbox")
	}
	return buckets, bins
}

// A fresh capture makes one active pile stamped with the capture's style, and
// the flush carries a +qty capture_fill for it and a -qty capture_reduction for
// the released bin.
// Flips under change #1: the pile's style stamp goes, and the bucket message
// becomes the row's level instead of a +qty delta. The bin half stays.
func TestCaptureToLineside_FreshCaptureMakesPileAndPairedDeltas(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-FRESH")

	if got := f.capture(t, f.styleB, 901, false, map[string]int{"SYN-PART-1": 12}); got != 12 {
		t.Fatalf("capturedTotal = %d, want 12", got)
	}

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].State != lineside.StateActive || rows[0].Qty != 12 ||
		rows[0].StyleID != f.styleB || rows[0].PayloadCode != "SYN-PART-1" {
		t.Fatalf("rows = %+v, want one active SYN-PART-1 pile of 12 stamped style %d", rows, f.styleB)
	}
	buckets, bins := f.flushed(t)
	if len(buckets) != 1 || buckets[0].Delta != 12 || buckets[0].Reason != protocol.ReasonCaptureFill ||
		buckets[0].StyleID != f.styleB || buckets[0].CoreNodeName != f.coreSeat || buckets[0].PayloadCode != "SYN-PART-1" {
		t.Errorf("bucket deltas = %+v, want one capture_fill +12 for SYN-PART-1 under style %d", buckets, f.styleB)
	}
	if len(bins) != 1 || bins[0].BinID != 901 || bins[0].Delta != -12 || bins[0].Reason != protocol.ReasonCaptureReduction {
		t.Errorf("bin deltas = %+v, want one capture_reduction -12 against bin 901", bins)
	}
}

// A second capture of the same part under the same style folds into the one
// active pile; the flush carries only the second capture's qty.
// Flips under change #1: the message becomes the row's level (20), not +8.
func TestCaptureToLineside_SameStyleMergesIntoActivePile(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-MERGE")
	f.capture(t, f.styleA, 902, false, map[string]int{"SYN-PART-1": 12})
	f.flushed(t)

	f.capture(t, f.styleA, 903, false, map[string]int{"SYN-PART-1": 8})

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Qty != 20 || rows[0].State != lineside.StateActive {
		t.Fatalf("rows = %+v, want one active pile of 20", rows)
	}
	buckets, bins := f.flushed(t)
	if len(buckets) != 1 || buckets[0].Delta != 8 {
		t.Errorf("bucket deltas = %+v, want one +8 (the delta, not the pile's 20)", buckets)
	}
	if len(bins) != 1 || bins[0].BinID != 903 || bins[0].Delta != -8 {
		t.Errorf("bin deltas = %+v, want -8 against bin 903", bins)
	}
}

// A capture under another style folds into the existing active pile and
// RE-STAMPS it with the new style, while the delta goes out under the new style
// for only the new qty. This is the mechanism of the Core drift (FINDINGS §2):
// Core's (seat, A, part) row keeps the first 12 and a new (seat, B, part) row
// gets 5, while the Edge holds one pile of 17 stamped B.
// Flips under change #1: one (seat, part) identity; the message is the level 17.
func TestCaptureToLineside_CrossStyleRestampsTheActivePile(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-XSTYLE")
	f.capture(t, f.styleA, 904, false, map[string]int{"SYN-PART-1": 12})
	f.flushed(t)

	f.capture(t, f.styleB, 905, false, map[string]int{"SYN-PART-1": 5})

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Qty != 17 || rows[0].StyleID != f.styleB || rows[0].State != lineside.StateActive {
		t.Fatalf("rows = %+v, want one active pile of 17 re-stamped to style %d", rows, f.styleB)
	}
	buckets, _ := f.flushed(t)
	if len(buckets) != 1 || buckets[0].Delta != 5 || buckets[0].StyleID != f.styleB {
		t.Errorf("bucket deltas = %+v, want one +5 under style %d", buckets, f.styleB)
	}
}

// A capture revives an INACTIVE pile of the same part, whatever style the
// capture is under: the stranded qty comes back into the active pile, and
// nothing is sent for the revived qty (only the new capture's +qty).
// Flips under change #2: a stranded pile never revives; the capture makes a new
// active row of 4 and the stranded row keeps its 30.
func TestCaptureToLineside_RevivesAnInactivePileUnderAnyStyle(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-REVIVE")
	f.capture(t, f.styleA, 906, false, map[string]int{"SYN-PART-1": 30})
	// A release under style B flips A's pile inactive (no parts captured).
	f.capture(t, f.styleB, 907, false, nil)
	if rows := f.rows(t); len(rows) != 1 || rows[0].State != lineside.StateInactive {
		t.Fatalf("setup: rows = %+v, want A's pile inactive", rows)
	}
	f.flushed(t)

	// A third style pulls the same part.
	f.capture(t, f.styleC, 908, false, map[string]int{"SYN-PART-1": 4})

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].State != lineside.StateActive || rows[0].Qty != 34 || rows[0].StyleID != f.styleC {
		t.Fatalf("rows = %+v, want one active pile of 34 (30 revived + 4) stamped style %d", rows, f.styleC)
	}
	buckets, _ := f.flushed(t)
	if len(buckets) != 1 || buckets[0].Delta != 4 {
		t.Errorf("bucket deltas = %+v, want one +4 — the revived 30 is sent nowhere", buckets)
	}
}

// Every release deactivates the node's active piles of OTHER styles (the
// capture's style is kept), and sends nothing for them: Core's mirror still
// counts them.
// Flips under change #2: DeactivateOtherStyles is deleted; a release leaves the
// other piles active and the cutover strands them (with a level message).
func TestCaptureToLineside_ReleaseDeactivatesOtherStylesSilently(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-DEACT")
	f.capture(t, f.styleA, 909, false, map[string]int{"SYN-PART-1": 9})
	f.flushed(t)

	f.capture(t, f.styleB, 910, false, map[string]int{"SYN-PART-2": 3})

	byPart := map[string]lineside.Bucket{}
	for _, r := range f.rows(t) {
		byPart[r.PayloadCode] = r
	}
	if r := byPart["SYN-PART-1"]; r.State != lineside.StateInactive || r.Qty != 9 {
		t.Errorf("style A pile = %+v, want inactive with its 9", r)
	}
	if r := byPart["SYN-PART-2"]; r.State != lineside.StateActive || r.Qty != 3 {
		t.Errorf("style B pile = %+v, want active 3", r)
	}
	buckets, _ := f.flushed(t)
	if len(buckets) != 1 || buckets[0].PayloadCode != "SYN-PART-2" || buckets[0].Delta != 3 {
		t.Errorf("bucket deltas = %+v, want only SYN-PART-2 +3 — nothing for the deactivated pile", buckets)
	}
}

// The supply leg of a two-robot swap (SuppressBinDelta) still makes the pile and
// sends its capture_fill, but sends no capture_reduction: parts appear on the
// bench that no bin paid for.
// Flips under change #4: a supply-leg pull makes no pile and sends nothing.
func TestCaptureToLineside_SupplyLegMakesPileWithoutBinDelta(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-SUPPLY")

	if got := f.capture(t, f.styleA, 911, true, map[string]int{"SYN-PART-1": 6}); got != 6 {
		t.Fatalf("capturedTotal = %d, want 6", got)
	}

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Qty != 6 || rows[0].State != lineside.StateActive {
		t.Fatalf("rows = %+v, want one active pile of 6", rows)
	}
	buckets, bins := f.flushed(t)
	if len(buckets) != 1 || buckets[0].Delta != 6 || buckets[0].Reason != protocol.ReasonCaptureFill {
		t.Errorf("bucket deltas = %+v, want one capture_fill +6", buckets)
	}
	if len(bins) != 0 {
		t.Errorf("bin deltas = %+v, want none on the supply leg", bins)
	}
}
