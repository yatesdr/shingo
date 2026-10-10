package uop

import (
	"testing"

	"shingo/protocol"
)

// A capture and a consume tick in ONE flush window ship as one delta, and the
// reason it carries is what Core keys its capture-to-zero manifest clear on
// (shingo-core uop/applier.go, Reason == capture_reduction). A PULL PARTS
// LINESIDE release sends no count; the capture_reduction delta is the only
// writer of that bin's clear. So a capture whose reason a later tick overwrote
// leaves the carrier labelled at 0 (row L item 3, ruled in DECISIONS: the
// capture's reason holds for the window).
func TestPin_CaptureThenTickInOneWindowKeepsTheCapture(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil)
	r.acc.recordBin(42, "PART-A", -10, protocol.ReasonCaptureReduction, 3)
	r.acc.recordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 3)
	r.Flush()
	ds := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(ds) != 1 || ds[0].Delta != -11 || ds[0].Reason != protocol.ReasonCaptureReduction {
		t.Fatalf("got %+v, want one delta -11 carrying capture_reduction", ds)
	}
}

// The reverse order already ships the capture (green before and after), and a
// window of ticks alone stays a consume tick.
func TestPin_TickThenCaptureInOneWindowKeepsTheCapture(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil)
	r.acc.recordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 3)
	r.acc.recordBin(42, "PART-A", -10, protocol.ReasonCaptureReduction, 3)
	r.acc.recordBin(43, "PART-A", -2, protocol.ReasonConsumeTick, 3)
	r.Flush()
	for _, d := range pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta) {
		want := protocol.ReasonConsumeTick
		if d.BinID == 42 {
			want = protocol.ReasonCaptureReduction
		}
		if d.Reason != want {
			t.Errorf("bin %d reason %q, want %q", d.BinID, d.Reason, want)
		}
	}
}

// Several ticks after the capture in one window do not overwrite it either.
func TestPin_CaptureSurvivesSeveralTicks(t *testing.T) {
	t.Parallel()
	db := newReporterTestDB(t)
	r := New(db, "ALN_001", nil, nil)
	r.acc.recordBin(42, "PART-A", -10, protocol.ReasonCaptureReduction, 3)
	r.acc.recordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 3)
	r.acc.recordBin(42, "PART-A", -1, protocol.ReasonConsumeTick, 3)
	r.Flush()
	ds := pendingOutboxByType[protocol.BinUOPDelta](t, db, protocol.SubjectBinUOPDelta)
	if len(ds) != 1 || ds[0].Reason != protocol.ReasonCaptureReduction {
		t.Fatalf("got %+v, want the capture's reason after two ticks", ds)
	}
}
