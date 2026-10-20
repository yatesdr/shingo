package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// ── A3: an unbound seat whose carrier departed through Shingo holds, up to one pack ──
//
// The departure mark is lineside_source = departed, written by carrierLeft. On
// an unbound produce seat with no live order placing a carrier, ticks are held
// while the mark stands, capped at one pack (the claim's UOPCapacity, 132
// here). The first stroke that would take the pile past the pack drops the
// whole pile with one hold_dropped (no_carrier) row, clears the mark, and the
// seat logs (the no_bin episode) until the next landing. A seat with no
// Shingo departure logs, as before.

const betweenBinsPack = 132

// betweenBinsSeat is p2Seat with the pack set to betweenBinsPack and, when
// departed, the seat's last carrier recorded as departed through carrierLeft
// (the evac's departure).
func betweenBinsSeat(t *testing.T, departed bool) (eng *Engine, sink *fakeDeltaSink, processID, nodeID, styleID, claimID int64) {
	t.Helper()
	eng, sink, processID, nodeID, styleID, claimID = p2Seat(t, 0, "")
	_, err := eng.db.DB.Exec(`UPDATE payload_catalog SET uop_capacity=? WHERE code='WIDGET-A'`, betweenBinsPack)
	testutil.MustNoErr(t, err, "set the pack")
	if departed {
		testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "the carrier departs")
	}
	return eng, sink, processID, nodeID, styleID, claimID
}

func strokes(eng *Engine, processID, styleID int64, n int) {
	for range n {
		charSPTick(eng, processID, styleID, 1)
	}
}

func assertDeparted(t *testing.T, eng *Engine, nodeID int64, want bool) {
	t.Helper()
	if got := carrierGone(runtimeOf(t, eng, nodeID)); got != want {
		t.Errorf("departure mark = %t, want %t", got, want)
	}
}

// TestBetweenBins_QualityHoldEvacWithALateRequest — the evac's carrier departs,
// 20 strokes land on the unbound seat with nothing coming, the REQUEST comes a
// minute later, and the next bin binds: its first tick is +21 and no row is
// written.
func TestBetweenBins_QualityHoldEvacWithALateRequest(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, claimID := betweenBinsSeat(t, true)
	strokes(eng, processID, styleID, 20)
	assertSeat(t, eng, nodeID, 0, 20, "WIDGET-A")

	p2Supply(t, eng, claimID)
	bin := int64(7701)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the next bin binds")
	charSPTick(eng, processID, styleID, 1)

	assertSeat(t, eng, nodeID, 21, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 21 {
		t.Errorf("deltas = %+v, want one produce_tick of 21", sink.binCalls)
	}
	assertDrops(t, eng.db)
}

// TestBetweenBins_PastThePackDropsThePileAndLogs — the same with no REQUEST:
// 132 strokes are held; stroke 133 drops the pile with one no_carrier row of
// 132, clears the mark and is logged; the strokes after it are logged too; the
// next bind's first tick is +1, carrying the logged strokes as one no_bin row.
func TestBetweenBins_PastThePackDropsThePileAndLogs(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, _ := betweenBinsSeat(t, true)
	strokes(eng, processID, styleID, betweenBinsPack)
	assertSeat(t, eng, nodeID, 0, betweenBinsPack, "WIDGET-A")
	assertDrops(t, eng.db)

	charSPTick(eng, processID, styleID, 1)
	noCarrier := protocol.HoldDropped{Reason: protocol.HoldDropNoCarrier, Qty: betweenBinsPack,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"}
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, noCarrier)
	assertDeparted(t, eng, nodeID, false)

	strokes(eng, processID, styleID, 4)
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, noCarrier)

	bin := int64(7702)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the next bin binds")
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	assertDrops(t, eng.db, noCarrier, protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 5,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
}

// TestBetweenBins_ANeverBoundSeatLogs — no Shingo departure: the seat logs and
// the strokes are one no_bin row at the next bind; nothing is ever held.
func TestBetweenBins_ANeverBoundSeatLogs(t *testing.T) {
	t.Parallel()
	eng, _, processID, nodeID, styleID, _ := betweenBinsSeat(t, false)
	strokes(eng, processID, styleID, 3)
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db)

	bin := int64(7703)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "a bin binds")
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 3,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
}

// TestBetweenBins_TheTrialResumedAfterDaysOfForkTruck — the departed seat ran
// on fork truck for days: the pile was dropped at the pack long ago, the seat
// is in log mode through scans and strokes, and the first bind counts +1. No
// timer: elapsed time plays no part.
func TestBetweenBins_TheTrialResumedAfterDaysOfForkTruck(t *testing.T) {
	t.Parallel()
	eng, _, processID, nodeID, styleID, _ := betweenBinsSeat(t, true)
	sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}
	strokes(eng, processID, styleID, betweenBinsPack+1)
	for day := range 3 {
		strokes(eng, processID, styleID, 500)
		sm.tick(time.Now().Add(time.Duration(day+1) * 24 * time.Hour))
	}
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDeparted(t, eng, nodeID, false)

	bin := int64(7704)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the trial's first bin binds")
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
}

// TestBetweenBins_ALandingEndsLogMode — after the pile is dropped at the pack
// the seat logs; a bin lands and leaves through Shingo again, and the strokes
// after that departure are held once more.
func TestBetweenBins_ALandingEndsLogMode(t *testing.T) {
	t.Parallel()
	eng, _, processID, nodeID, styleID, _ := betweenBinsSeat(t, true)
	strokes(eng, processID, styleID, betweenBinsPack+1)
	assertDeparted(t, eng, nodeID, false)

	bin := int64(7705)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "a bin lands")
	charSPTick(eng, processID, styleID, 1)
	_, err := eng.db.DB.Exec(`UPDATE process_node_runtime_states SET active_bin_id=NULL, remaining_uop_cached=0 WHERE process_node_id=?`, nodeID)
	testutil.MustNoErr(t, err, "the bin's pointer goes")
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "and it departs through Shingo")

	strokes(eng, processID, styleID, 3)
	assertSeat(t, eng, nodeID, 0, 3, "WIDGET-A")
}

// TestBetweenBins_TheSweepKeepsADepartedSeatsPileWithinThePack — the scan's
// produce arm drops an unbound pile with no carrier coming, except the hold
// the tick itself would keep: a departed seat's pile within the pack stays; one
// past the pack (a pile from before the rule) is swept as before.
func TestBetweenBins_TheSweepKeepsADepartedSeatsPileWithinThePack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		held int
		kept bool
	}{
		{"within the pack", 20, true},
		{"at the pack", betweenBinsPack, true},
		{"past the pack", betweenBinsPack + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, _, _, nodeID, _, _ := betweenBinsSeat(t, true)
			_, err := eng.db.DB.Exec(`UPDATE process_node_runtime_states SET pending_uop_delta=?, pending_uop_payload='WIDGET-A' WHERE process_node_id=?`, tc.held, nodeID)
			testutil.MustNoErr(t, err, "seed the pile")
			sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}
			sm.tick(time.Now())
			if tc.kept {
				assertSeat(t, eng, nodeID, 0, int64(tc.held), "WIDGET-A")
				assertDrops(t, eng.db)
				return
			}
			assertSeat(t, eng, nodeID, 0, 0, "")
			assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropSweep, Qty: int64(tc.held),
				PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:sweep"})
		})
	}
}
