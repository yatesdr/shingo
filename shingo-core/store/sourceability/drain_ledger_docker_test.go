//go:build docker

package sourceability_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/service"
	"shingocore/store/plantclaims"
)

// B7 Fix 2's DB pins: the drain ledger end to end. The bucket deltas go
// through ApplyLinesideBucketDelta — the real writer — because what is being
// pinned is the door Core owns: which deltas leave a drain row, with which
// before/after, and what the rate makes of them.

// drainWorld stages the drain-only fixture: standard data, SNF2 style A
// (active) claiming BIN-A at LINE1-IN, a staged bin (the projection's
// numerator) and NO bin_uop_ledger rows — the cell's consumption is entirely
// bucket drains.
func drainWorld(t *testing.T) *rateWorld {
	t.Helper()
	w := setupRateWorld(t, 120, 0)

	// Drop the second-line claim: this fixture is one cell.
	claims := []plantclaims.ClaimRow{
		{ProcessID: "SNF2", StyleID: "A", CoreNodeName: w.std.LineNode.Name, PayloadCode: "BIN-A", Seq: 0},
	}
	styles := []plantclaims.StyleRow{{ProcessID: "SNF2", StyleID: "A", IsActive: true}}
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(w.db, "SNF2", styles, claims, 0), "reseed mirror one line")
	return w
}

// bucketDelta builds a LinesideBucketDelta in the wire shape makeBucketDelta
// uses (uop/applier_test.go), local so this file owns its fixtures.
func bucketDelta(node, payload string, delta int, seq int64, reason protocol.LinesideBucketDeltaReason) *protocol.LinesideBucketDelta {
	now := time.Now().UTC()
	return &protocol.LinesideBucketDelta{
		CoreNodeName: node, PairKey: "RATE|PAIR", StyleID: 100,
		PayloadCode: payload, Delta: delta, Reason: reason, SequenceID: seq,
		WindowStart: now.Add(-5 * time.Second), WindowEnd: now,
	}
}

func applyBucket(t *testing.T, w *rateWorld, delta *protocol.LinesideBucketDelta) {
	t.Helper()
	svc := service.NewInventoryDeltaService(w.sdb, service.NewBinManifestService(w.sdb, service.EpochAnnounce{}), service.EpochAnnounce{})
	testutil.MustNoErr(t, svc.ApplyLinesideBucketDelta("ALN_RATE", delta), "apply bucket delta")
}

func drainRows(t *testing.T, w *rateWorld) (count int, before, after int) {
	t.Helper()
	err := w.db.QueryRow(`SELECT count(*), COALESCE(MIN(before_qty),0), COALESCE(MIN(after_qty),0)
		FROM lineside_drain_ledger`).Scan(&count, &before, &after)
	testutil.MustNoErr(t, err, "read drain ledger")
	return count, before, after
}

// TestRate_DrainOnlyCellFinallyHasARate pins P7: a cell whose consumption is
// entirely lineside drains. Pre-v120 the rate read nothing (the dead
// OR op='lineside_drain' arm matched zero rows on the bin ledger), the
// projection was Known=false and no sample was worth scoring. Now the drain
// ledger feeds both grains: 60 units over the window at the node the pile
// sits at.
func TestRate_DrainOnlyCellFinallyHasARate(t *testing.T) {
	w := drainWorld(t)
	applyBucket(t, w, bucketDelta(w.std.LineNode.Name, "BIN-A", 60, 1, protocol.ReasonCaptureFill))
	applyBucket(t, w, bucketDelta(w.std.LineNode.Name, "BIN-A", -60, 2, protocol.ReasonConsumeDrain))

	// The writer's own contract: exactly one drain row, before=60 after=0.
	if n, before, after := drainRows(t, w); n != 1 || before != 60 || after != 0 {
		t.Fatalf("drain ledger = %d rows (before=%d after=%d), want 1 row before=60 after=0", n, before, after)
	}

	var binLedger int
	testutil.MustNoErr(t, w.db.QueryRow(`SELECT count(*) FROM bin_uop_ledger`).Scan(&binLedger), "bin ledger count")
	if binLedger != 0 {
		t.Fatalf("bin_uop_ledger holds %d rows in the drain-only fixture, want 0", binLedger)
	}

	_, samples := buildAndCompute(t, w)
	s := sampleFor(t, samples, w.std.LineNode.Name)
	if !s.Line.Known {
		t.Fatalf("line = %+v, want a known projection from the drain rate", s.Line)
	}
	if got := s.Line.RatePerSec; got < 60/w1800-1e-9 || got > 60/w1800+1e-9 {
		t.Errorf("rate = %v, want 60/1800 from the drain ledger alone", got)
	}
	if s.Line.RateGrain != "node" {
		t.Errorf("grain = %q, want node (the pile sits at a known node)", s.Line.RateGrain)
	}
	if s.Line.TimeToEmpty != 3600*time.Second {
		t.Errorf("TTE = %v, want 3600s (120 uop at 60/1800)", s.Line.TimeToEmpty)
	}
}

// TestRate_CaptureFillWritesNoDrainRow pins P9: the ledger door itself.
// capture_fill is parts arriving at a pile, not consumption — one fill and
// one drain at the same bucket must leave EXACTLY one row, the drain's,
// with before/after from the qty arithmetic the UPSERT performed.
func TestRate_CaptureFillWritesNoDrainRow(t *testing.T) {
	w := drainWorld(t)
	applyBucket(t, w, bucketDelta(w.std.LineNode.Name, "BIN-A", 47, 1, protocol.ReasonCaptureFill))
	applyBucket(t, w, bucketDelta(w.std.LineNode.Name, "BIN-A", -10, 2, protocol.ReasonConsumeDrain))

	if n, before, after := drainRows(t, w); n != 1 || before != 47 || after != 37 {
		t.Fatalf("drain ledger = %d rows (before=%d after=%d), want exactly the consume_drain row before=47 after=37", n, before, after)
	}

	var reason string
	testutil.MustNoErr(t, w.db.QueryRow(`SELECT reason FROM lineside_drain_ledger`).Scan(&reason), "read reason")
	if reason != "consume_drain" {
		t.Errorf("reason = %q, want consume_drain", reason)
	}
}

// TestRate_DrainsFoldIntoBothGrains pins the reader's UNION arm at the map
// grain: a cell with BOTH tick consumption and a drain (the real mixed shape
// once a bin sits above a pile) sums the two sources at the node, and the
// payload sum keeps both too.
func TestRate_DrainsFoldIntoBothGrains(t *testing.T) {
	w := drainWorld(t)
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "consume_tick", 60, 0)
	applyBucket(t, w, bucketDelta(w.std.LineNode.Name, "BIN-A", 30, 1, protocol.ReasonCaptureFill))
	applyBucket(t, w, bucketDelta(w.std.LineNode.Name, "BIN-A", -30, 2, protocol.ReasonConsumeDrain))

	_, samples := buildAndCompute(t, w)
	s := sampleFor(t, samples, w.std.LineNode.Name)
	// 60 tick units + 30 drain units = 90/1800 at the node.
	if got := s.Line.RatePerSec; got < 90/w1800-1e-9 || got > 90/w1800+1e-9 {
		t.Errorf("node rate = %v, want 90/1800 (ticks + drain)", got)
	}
	if s.Line.RateGrain != "node" {
		t.Errorf("grain = %q, want node", s.Line.RateGrain)
	}
}
