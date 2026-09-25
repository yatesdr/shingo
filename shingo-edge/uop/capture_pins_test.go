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
// accumulator puts on the wire for them (the pile's level and the bin's
// capture_reduction). Pinned before the bucket-level change; each test names
// the expected change it flipped under, or says it stays.

type captureFixture struct {
	db       *store.DB
	m        *Mutator
	procID   int64
	nodeID   int64
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
	styleID, err := db.CreateStyle(prefix+"-A", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")
	return &captureFixture{db: db, m: New(db, "stn-test", db, db), procID: procID, nodeID: nodeID, coreSeat: prefix + "-SEAT"}
}

func (f *captureFixture) capture(t *testing.T, binID int64, suppressBin bool, qty map[string]int) int {
	t.Helper()
	n, err := f.m.CaptureToLineside(CaptureEvent{
		NodeID: f.nodeID, CoreNodeName: f.coreSeat,
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

func (f *captureFixture) flushed(t *testing.T) ([]protocol.LinesideBucketLevel, []protocol.BinUOPDelta) {
	t.Helper()
	f.m.Flush()
	levels := pendingOutboxByType[protocol.LinesideBucketLevel](t, f.db, protocol.SubjectLinesideBucketLevel)
	bins := pendingOutboxByType[protocol.BinUOPDelta](t, f.db, protocol.SubjectBinUOPDelta)
	msgs, err := f.db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	for _, msg := range msgs {
		testutil.MustNoErr(t, f.db.AckOutbox(msg.ID), "ack outbox")
	}
	return levels, bins
}

// A fresh capture makes one active pile, and the flush carries its level (12,
// active) and a -qty capture_reduction for the released bin.
// Flipped under change #1: the pile has no style stamp, and the bucket message
// is the row's level instead of a +qty capture_fill delta. The bin half stays.
func TestCaptureToLineside_FreshCaptureSendsLevelAndBinDelta(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-FRESH")

	if got := f.capture(t, 901, false, map[string]int{"SYN-PART-1": 12}); got != 12 {
		t.Fatalf("capturedTotal = %d, want 12", got)
	}

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].State != lineside.StateActive || rows[0].Qty != 12 || rows[0].PayloadCode != "SYN-PART-1" {
		t.Fatalf("rows = %+v, want one active SYN-PART-1 pile of 12", rows)
	}
	levels, bins := f.flushed(t)
	if len(levels) != 1 || levels[0].Qty != 12 || levels[0].State != protocol.LinesideBucketActive ||
		levels[0].Drained != 0 || levels[0].CoreNodeName != f.coreSeat || levels[0].PayloadCode != "SYN-PART-1" {
		t.Errorf("levels = %+v, want one active level of 12 for %s/SYN-PART-1, Drained 0 (a pull is not consumption)", levels, f.coreSeat)
	}
	if len(bins) != 1 || bins[0].BinID != 901 || bins[0].Delta != -12 || bins[0].Reason != protocol.ReasonCaptureReduction {
		t.Errorf("bin deltas = %+v, want one capture_reduction -12 against bin 901", bins)
	}
}

// A second capture of the same part folds into the one active pile; the flush
// carries the pile's level.
// Flipped under change #1: the message is the row's level (20), not +8.
func TestCaptureToLineside_SecondCaptureSendsThePilesLevel(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-MERGE")
	f.capture(t, 902, false, map[string]int{"SYN-PART-1": 12})
	f.flushed(t)

	f.capture(t, 903, false, map[string]int{"SYN-PART-1": 8})

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Qty != 20 || rows[0].State != lineside.StateActive {
		t.Fatalf("rows = %+v, want one active pile of 20", rows)
	}
	levels, bins := f.flushed(t)
	if len(levels) != 1 || levels[0].Qty != 20 || levels[0].SequenceID != 2 {
		t.Errorf("levels = %+v, want one level of 20 at seq 2 (the row, not the +8)", levels)
	}
	if len(bins) != 1 || bins[0].BinID != 903 || bins[0].Delta != -8 {
		t.Errorf("bin deltas = %+v, want -8 against bin 903", bins)
	}
}

// Captures made while the process runs different styles fold into the same
// one active pile: there is one (seat, part) identity.
// Flipped under change #1: the capture used to re-stamp the pile with the new
// style and send +5 under it, which split Core's copy by style (FINDINGS §2);
// the capture has no style now and the message is the level 17.
func TestCaptureToLineside_CapturesAcrossStylesAreOnePile(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-XSTYLE")
	f.capture(t, 904, false, map[string]int{"SYN-PART-1": 12})
	f.flushed(t)
	other, err := f.db.CreateStyle("CAP-XSTYLE-B", "", f.procID)
	testutil.MustNoErr(t, err, "create style B")
	// The style flips WITHOUT a cutover (a raw store write): only a cutover
	// strands, and this pins that the capture itself has no style.
	testutil.MustNoErr(t, f.db.SetActiveStyle(f.procID, &other), "set style B")

	f.capture(t, 905, false, map[string]int{"SYN-PART-1": 5})

	rows := f.rows(t)
	if len(rows) != 1 || rows[0].Qty != 17 || rows[0].State != lineside.StateActive {
		t.Fatalf("rows = %+v, want one active pile of 17", rows)
	}
	levels, _ := f.flushed(t)
	if len(levels) != 1 || levels[0].Qty != 17 {
		t.Errorf("levels = %+v, want one level of 17", levels)
	}
}

// A capture never revives a stranded pile of the same part: the stranded row
// keeps its qty, the pull makes a new active pile, and the flush sends only the
// active level.
// Flipped under change #2: the capture used to fold the inactive 30 back into
// the active pile (34) and send only +4, so the revived 30 reached Core nowhere.
func TestCaptureToLineside_NeverRevivesAStrandedPile(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-REVIVE")
	f.capture(t, 906, false, map[string]int{"SYN-PART-1": 30})
	_, err := f.db.StrandLinesidePiles(f.procID) // the cutover
	testutil.MustNoErr(t, err, "strand")
	f.flushed(t)

	f.capture(t, 908, false, map[string]int{"SYN-PART-1": 4})

	byState := map[string]int{}
	for _, r := range f.rows(t) {
		byState[r.State] = r.Qty
	}
	if byState[lineside.StateActive] != 4 || byState[lineside.StateStranded] != 30 {
		t.Fatalf("piles = %v, want active 4 beside stranded 30", byState)
	}
	levels, _ := f.flushed(t)
	if len(levels) != 1 || levels[0].State != protocol.LinesideBucketActive || levels[0].Qty != 4 {
		t.Errorf("levels = %+v, want one active level of 4", levels)
	}
}

// A release no longer touches the node's other piles: they stay active until
// the cutover, and the flush sends only the captured pile's level.
// Flipped under change #2: DeactivateOtherStyles is deleted. A release used to
// flip other-style piles inactive and send nothing for them.
func TestCaptureToLineside_ReleaseLeavesOtherPilesActive(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-DEACT")
	f.capture(t, 909, false, map[string]int{"SYN-PART-1": 9})
	f.flushed(t)

	f.capture(t, 910, false, map[string]int{"SYN-PART-2": 3})

	byPart := map[string]lineside.Bucket{}
	for _, r := range f.rows(t) {
		byPart[r.PayloadCode] = r
	}
	if r := byPart["SYN-PART-1"]; r.State != lineside.StateActive || r.Qty != 9 {
		t.Errorf("first pile = %+v, want still active with its 9", r)
	}
	if r := byPart["SYN-PART-2"]; r.State != lineside.StateActive || r.Qty != 3 {
		t.Errorf("second pile = %+v, want active 3", r)
	}
	levels, _ := f.flushed(t)
	if len(levels) != 1 || levels[0].PayloadCode != "SYN-PART-2" || levels[0].Qty != 3 {
		t.Errorf("levels = %+v, want only SYN-PART-2's level of 3", levels)
	}
}

// The supply leg of a two-robot swap (SuppressBinDelta) captures nothing: no
// pile, no level, no capture_reduction. No bin paid for the parts.
// Flipped under change #4: it used to make the pile and send its capture_fill
// while sending no capture_reduction, so parts appeared on the bench that no bin
// paid for.
func TestCaptureToLineside_SupplyLegMakesNoPile(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t, "CAP-SUPPLY")

	if got := f.capture(t, 911, true, map[string]int{"SYN-PART-1": 6}); got != 0 {
		t.Fatalf("capturedTotal = %d, want 0", got)
	}

	if rows := f.rows(t); len(rows) != 0 {
		t.Fatalf("rows = %+v, want no pile on the supply leg", rows)
	}
	levels, bins := f.flushed(t)
	if len(levels) != 0 || len(bins) != 0 {
		t.Errorf("levels = %+v, bins = %+v, want nothing on the supply leg", levels, bins)
	}
}
