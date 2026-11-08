package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// ── The produce arm: the sweep of a standing pile with no carrier coming ──
//
// Each 60 s scan, and once at boot, an UNBOUND produce seat with a hold and no
// live order placing a carrier at the node has its hold dropped, guarded on
// the seat as the scan read it, with one hold_dropped (sweep) row. A bound
// seat is never touched. Unless a pin says otherwise the seat holds 3,000 of
// WIDGET-A (pack 100) with no bin bound: the ALN_010 shape.

// sweepSeat is p2Seat with the stranded monitor that scans it.
func sweepSeat(t *testing.T, held int) (eng *Engine, sink *fakeDeltaSink, sm *strandedMonitor, processID, nodeID, styleID, claimID int64) {
	t.Helper()
	eng, sink, processID, nodeID, styleID, claimID = p2Seat(t, held, "WIDGET-A")
	return eng, sink, &strandedMonitor{eng: eng}, processID, nodeID, styleID, claimID
}

var sweepDrop = protocol.HoldDropped{Reason: protocol.HoldDropSweep, Qty: 3000, PayloadCode: "WIDGET-A",
	Node: "PRODUCE-NODE", CalledBy: "edge:sweep"}

// TestProduceHoldSweep_BootDropsAStandingPileAndTheNextBindTakesOne — the
// ALN_010 shape and the rollout write-off: a bin-less pile of 3,000 with no
// live order is dropped by the boot pass with one sweep row, and the next bin
// bound takes the next stroke alone (+1, not +3,001).
func TestProduceHoldSweep_BootDropsAStandingPileAndTheNextBindTakesOne(t *testing.T) {
	t.Parallel()
	eng, sink, sm, processID, nodeID, styleID, _ := sweepSeat(t, 3000)

	sm.sweepAtBoot()
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, sweepDrop)

	bin := int64(7501)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the next bin binds")
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
}

// TestProduceHoldSweep_ClosesANoBinEpisodeOnABoundSeat — strokes logged on an
// unbound seat with nothing coming stay open through a scan; a bin binds and,
// with no tick after it, the next scan sends the episode as one no_bin row of
// 5. The bin's first tick then counts +1 and sends nothing more.
func TestProduceHoldSweep_ClosesANoBinEpisodeOnABoundSeat(t *testing.T) {
	t.Parallel()
	eng, sink, sm, processID, nodeID, styleID, _ := sweepSeat(t, 0)
	charSPTick(eng, processID, styleID, 2)
	charSPTick(eng, processID, styleID, 3)
	sm.tick(time.Now())
	assertDrops(t, eng.db)

	bin := int64(7602)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the next bin binds")
	sm.tick(time.Now())
	noBin := protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 5, PayloadCode: "WIDGET-A",
		Node: "PRODUCE-NODE", CalledBy: "edge:sweep"}
	assertDrops(t, eng.db, noBin)

	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	assertDrops(t, eng.db, noBin)
}

// TestProduceHoldSweep_TheScanDropsAStandingPile — the same drop from the 60 s
// scan (strandedMonitor.tick), before its consume-only skip.
func TestProduceHoldSweep_TheScanDropsAStandingPile(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, _ := sweepSeat(t, 3000)
	sm.tick(time.Now())
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, sweepDrop)
}

// TestProduceHoldSweep_KeepsAPileACarrierIsComingFor — a live supply leg
// placing a carrier at the node: the hold is the next bin's, and stays.
func TestProduceHoldSweep_KeepsAPileACarrierIsComingFor(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, claimID := sweepSeat(t, 3000)
	p2Supply(t, eng, claimID)
	sm.sweepAtBoot()
	sm.tick(time.Now())
	assertSeat(t, eng, nodeID, 0, 3000, "WIDGET-A")
	assertDrops(t, eng.db)
}

// TestProduceHoldSweep_NeverTouchesABoundSeat — a bucket beside a bound bin (a
// bin at its pack waiting for a late REQUEST) is the next bin's whether or not
// an order exists yet; the sweep skips every bound seat.
func TestProduceHoldSweep_NeverTouchesABoundSeat(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, _ := sweepSeat(t, 30)
	bin := int64(7502)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "a bin at the seat")
	sm.sweepAtBoot()
	sm.tick(time.Now())
	assertSeat(t, eng, nodeID, 0, 30, "WIDGET-A")
	assertDrops(t, eng.db)
}

// TestProduceHoldSweep_ASeatChangedSinceTheScanReadItRefusesTheDrop — the scan
// read the seat unbound with 3,000 held; a bin binds before the drop takes the
// count lock. The drop names the seat as read, so it is refused: no change, no
// row.
func TestProduceHoldSweep_ASeatChangedSinceTheScanReadItRefusesTheDrop(t *testing.T) {
	t.Parallel()
	eng, _, _, _, nodeID, _, claimID := sweepSeat(t, 3000)
	node, err := eng.db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	stale := runtimeOf(t, eng, nodeID)
	bin := int64(7503)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "a bin binds after the read")

	eng.sweepProduceHold(node, p2Claim(t, eng, claimID), stale)
	assertSeat(t, eng, nodeID, 0, 3000, "WIDGET-A")
	assertDrops(t, eng.db)
}

// TestProduceHoldSweep_AnUnreadableOrderKeepsThePile — a live leg whose release
// facts do not parse may be placing a carrier here: no drop.
func TestProduceHoldSweep_AnUnreadableOrderKeepsThePile(t *testing.T) {
	t.Parallel()
	eng, _, sm, _, nodeID, _, claimID := sweepSeat(t, 3000)
	id := p2Supply(t, eng, claimID)
	_, err := eng.db.DB.Exec(`UPDATE orders SET release_facts='{"places":' WHERE id=?`, id)
	testutil.MustNoErr(t, err, "corrupt the facts")
	sm.sweepAtBoot()
	assertSeat(t, eng, nodeID, 0, 3000, "WIDGET-A")
	assertDrops(t, eng.db)
}
