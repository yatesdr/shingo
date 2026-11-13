package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// ── A3: an unbound seat whose carrier departed through Shingo holds, up to one pack ──
//
// The departure mark is lineside_source = departed, written by carrierLeft. On
// an unbound produce seat with no live order placing a carrier, ticks are held
// while the mark stands, capped at one pack (the claim's UOPCapacity, 132
// here). The first stroke that would take the pile past the pack throws the
// whole pile away with one hold_dropped (hold_cap) row, clears the mark and
// stops the seat, which sets nothing aside (the no_bin stretch) until another
// cart binds or a person counts (D2/D3, U1). A seat with no Shingo departure
// logs, as before.

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

// TestBetweenBins_PastThePackThrowsThePileAwayAndStops — the same with no
// REQUEST: 132 strokes are held; stroke 133 throws the whole pile away with one
// hold_cap row of 132 (D2), stops the seat and clears the mark, and the stroke
// joins the no-cart stretch; the strokes after it do too, even once a carrier
// is coming (D3). The next bind clears the stop: its first tick is +1,
// carrying the stretch as one no_bin row of 6. (Stroke 133 dropped the pile
// as no_carrier and the seat logged until the next landing, until ALN_011.)
func TestBetweenBins_PastThePackThrowsThePileAwayAndStops(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, claimID := betweenBinsSeat(t, true)
	strokes(eng, processID, styleID, betweenBinsPack)
	assertSeat(t, eng, nodeID, 0, betweenBinsPack, "WIDGET-A")
	assertDrops(t, eng.db)

	charSPTick(eng, processID, styleID, 1)
	holdCapRow := protocol.HoldDropped{Reason: protocol.HoldDropHoldCap, Qty: betweenBinsPack,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"}
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, holdCapRow)
	assertDeparted(t, eng, nodeID, false)
	if !runtimeOf(t, eng, nodeID).HoldStopped {
		t.Error("the seat is not stopped after the throw-away")
	}

	strokes(eng, processID, styleID, 4)
	p2Supply(t, eng, claimID)
	strokes(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, holdCapRow)

	bindCart(t, eng, nodeID, 7702)
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	assertDrops(t, eng.db, holdCapRow, protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 6,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
}

// bindCart binds bin at the seat as a landing of another cart (BindLanding),
// whose statement lifts the stop flag (U1); SetProcessNodeActiveBinID is a
// fixture pointer write and lifts nothing.
func bindCart(t *testing.T, eng *Engine, nodeID, bin int64) {
	t.Helper()
	seq := int64(1<<40) + bin
	_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: bin, Epoch: 1, PlacedSeq: &seq})
	testutil.MustNoErr(t, err, "a cart lands")
}

// TestThrowAway_ACarrierComingNoLongerGrowsThePile — the ALN_011 night's arm
// (D1-D3): an unbound seat with a supply leg placing a carrier here held every
// stroke, uncapped (794 by morning). Now it holds up to one pack; the stroke
// past it throws the whole pile away with one hold_cap row and stops the seat,
// and the strokes after it join the no-cart stretch though the carrier is
// still coming. The supply's cart lands: the stop clears, counting resumes
// onto it at 0, and the stretch goes as one no_bin row.
func TestThrowAway_ACarrierComingNoLongerGrowsThePile(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, claimID := betweenBinsSeat(t, false)
	p2Supply(t, eng, claimID)
	strokes(eng, processID, styleID, betweenBinsPack)
	assertSeat(t, eng, nodeID, 0, betweenBinsPack, "WIDGET-A")

	strokes(eng, processID, styleID, 1+10)
	assertSeat(t, eng, nodeID, 0, 0, "")
	if !runtimeOf(t, eng, nodeID).HoldStopped {
		t.Error("the seat is not stopped after the throw-away")
	}
	holdCapRow := protocol.HoldDropped{Reason: protocol.HoldDropHoldCap, Qty: betweenBinsPack,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"}
	assertDrops(t, eng.db, holdCapRow)

	bindCart(t, eng, nodeID, 7710)
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	assertDrops(t, eng.db, holdCapRow, protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 11,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
}

// TestThrowAway_AfterLiftTicksPastOnePack — the ticks counted after a late-heard
// lift join the seat's hold for the next cart (Lane H R4) only while the pile
// stays within one pack: 130 held and 4 more would pass 132, so the pile of 130
// is thrown away (hold_cap) in its own transaction before the leave, the seat
// stops, and the 4 join the no-cart stretch. On a stopped seat they join it
// directly.
func TestThrowAway_AfterLiftTicksPastOnePack(t *testing.T) {
	t.Parallel()
	for _, stopped := range []bool{false, true} {
		db := testEngineDB(t)
		_, nodeID, _, claimID := seedProduceNode(t, db, protocol.SwapModeTwoRobot)
		_, err := db.DB.Exec(`UPDATE payload_catalog SET uop_capacity=? WHERE code='WIDGET-A'`, betweenBinsPack)
		testutil.MustNoErr(t, err, "set the pack")
		bin := int64(7711)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &bin, 3, betweenBinsPack), "bind at generation 3")
		if !stopped {
			testutil.MustNoErr(t, processes.AddPendingUOPDelta(db.DB, nodeID, 130, "WIDGET-A"), "the pile beside the full cart")
		} else {
			testutil.MustNoErr(t, processes.StopSeatHold(db.DB, nodeID), "a stopped seat")
		}
		eng := testEngine(t, db)
		eng.SetInventoryDeltaSink(&afterLiftSink{sum: 4})
		node, err := db.GetProcessNode(nodeID)
		testutil.MustNoErr(t, err, "read node")

		held, part := eng.takeTicksAfterLift(node, runtimeOf(t, eng, nodeID), protocol.BinPickedUp{BinID: bin, Epoch: 4, PickedUpAt: time.Now()})
		if held != 0 || part != "" {
			t.Errorf("stopped %t: hold for the leave = %d of %q, want none", stopped, held, part)
		}
		rt := runtimeOf(t, eng, nodeID)
		if rt.PendingUOPDelta != 0 || !rt.HoldStopped {
			t.Errorf("stopped %t: seat hold %d stopped %t, want 0 and stopped", stopped, rt.PendingUOPDelta, rt.HoldStopped)
		}
		if got := eng.noBinRows(nodeID); len(got) != 1 || got[0].Qty != 4 {
			t.Errorf("stopped %t: the no-cart stretch = %+v, want one row of 4", stopped, got)
		}
		if stopped {
			assertDrops(t, db)
			continue
		}
		assertDrops(t, db, protocol.HoldDropped{Reason: protocol.HoldDropHoldCap, Qty: 130, PayloadCode: "WIDGET-A",
			Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
	}
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
	sm := &strandedMonitor{eng: eng}
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

// TestBetweenBins_ALandingEndsLogMode — after the pile is thrown away past the
// pack the seat is stopped; a cart binds (which clears the stop) and leaves
// through Shingo again, and the strokes after that departure are held once
// more.
func TestBetweenBins_ALandingEndsLogMode(t *testing.T) {
	t.Parallel()
	eng, _, processID, nodeID, styleID, _ := betweenBinsSeat(t, true)
	strokes(eng, processID, styleID, betweenBinsPack+1)
	assertDeparted(t, eng, nodeID, false)

	bindCart(t, eng, nodeID, 7705)
	charSPTick(eng, processID, styleID, 1)
	_, err := eng.db.DB.Exec(`UPDATE process_node_runtime_states SET active_bin_id=NULL, remaining_uop_cached=0 WHERE process_node_id=?`, nodeID)
	testutil.MustNoErr(t, err, "the bin's pointer goes")
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "and it departs through Shingo")

	strokes(eng, processID, styleID, 3)
	assertSeat(t, eng, nodeID, 0, 3, "WIDGET-A")
}

// TestHoldCap_EverySiteStopsAtTheSameNumber — D1: one cap. Every site that
// decides "past one pack" reads holdCap (the claim's UOPCapacity), on a strict
// claim and on one without strict_pack alike: at the cap the pile is kept, and
// one stroke past it nothing more is set aside. The pure rows are the deciders;
// the engine rows are the tick and the sweep reading the number off the claim.
func TestHoldCap_EverySiteStopsAtTheSameNumber(t *testing.T) {
	t.Parallel()
	if got := holdCap(nil); got != 0 {
		t.Errorf("holdCap(nil) = %d, want 0: no claim, no pack", got)
	}
	for _, strict := range []bool{true, false} {
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce, PayloadCode: "WIDGET-A",
			StrictPack: strict, UOPCapacity: betweenBinsPack}
		capN := holdCap(claim)
		if capN != betweenBinsPack {
			t.Fatalf("strict %t: holdCap = %d, want the claim's pack %d", strict, capN, betweenBinsPack)
		}
		departed := &processes.RuntimeState{LinesideSource: string(domain.CarrierDeparted),
			PendingUOPDelta: int64(capN), PendingUOPPayload: "WIDGET-A"}
		if hold, past := betweenBins(departed, "WIDGET-A", capN, 0); !hold || past {
			t.Errorf("strict %t: betweenBins at the cap = hold %t past %t, want kept", strict, hold, past)
		}
		if hold, past := betweenBins(departed, "WIDGET-A", capN, 1); hold || !past {
			t.Errorf("strict %t: betweenBins one past the cap = hold %t past %t, want past the pack", strict, hold, past)
		}
		if c := splitFlipCarry(capN+5, false, 0, 0, "", "WIDGET-A", strictPackFor(claim), capN); c.hold > capN {
			t.Errorf("strict %t: a flip carry onto an unbound seat holds %d, want at most %d", strict, c.hold, capN)
		}
		if strict {
			if s := splitStrict(capN, capN, "WIDGET-A", "WIDGET-A", 1, strictPackFor(claim), capN); s.hold > capN {
				t.Errorf("a full strict bin's hold after one more stroke = %d, want at most %d", s.hold, capN)
			}
		}

		eng, _, processID, nodeID, styleID, claimID := betweenBinsSeat(t, true)
		_, err := eng.db.DB.Exec(`UPDATE style_node_claims SET strict_pack=? WHERE id=?`, strict, claimID)
		testutil.MustNoErr(t, err, "set strict_pack")
		strokes(eng, processID, styleID, capN)
		assertSeat(t, eng, nodeID, 0, int64(capN), "WIDGET-A")
		(&strandedMonitor{eng: eng}).tick(time.Now())
		assertSeat(t, eng, nodeID, 0, int64(capN), "WIDGET-A")
		strokes(eng, processID, styleID, 1)
		if rt := runtimeOf(t, eng, nodeID); rt.PendingUOPDelta > int64(capN) {
			t.Errorf("strict %t: the tick past the cap left a hold of %d, want at most %d", strict, rt.PendingUOPDelta, capN)
		}
	}
}

// TestHoldStopped_ReadsBackAndEveryBindOfAnotherCartClearsIt — the stop flag
// (hold_stopped, D3) round-trips through the runtime read, a seat write that
// read the seat before it was set is refused (SeatAsRead names it), and the
// bind statements that lift it in their own SET do: a landing of another bin
// and a staged bind. A re-landing of the bin already bound keeps it (H.4), and
// so does the hand load's and Core's count statement on its own: who acted
// decides there, and the engine lifts it for a hand load or a person's count
// (U1; TestHoldStopped_WhatLiftsTheStop).
func TestHoldStopped_ReadsBackAndEveryBindOfAnotherCartClearsIt(t *testing.T) {
	t.Parallel()
	stop := func(t *testing.T, eng *Engine, nodeID int64) {
		t.Helper()
		_, err := eng.db.DB.Exec(`UPDATE process_node_runtime_states SET hold_stopped=1 WHERE process_node_id=?`, nodeID)
		testutil.MustNoErr(t, err, "stop the seat")
		if !runtimeOf(t, eng, nodeID).HoldStopped {
			t.Fatal("hold_stopped set, but the runtime read says it is not")
		}
	}
	seq := int64(1 << 40)
	for _, tc := range []struct {
		name    string
		bound   bool // bin 7801 bound before the bind
		bind    func(eng *Engine, nodeID, claimID int64) error
		stopped bool
	}{
		{name: "a landing of another bin", bind: func(eng *Engine, nodeID, _ int64) error {
			_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: 7802, Epoch: 1, PlacedSeq: &seq})
			return err
		}},
		{name: "a re-landing of the bin bound", bound: true, stopped: true, bind: func(eng *Engine, nodeID, _ int64) error {
			_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: 7801, Epoch: 1, PlacedSeq: &seq})
			return err
		}},
		{name: "the hand load's and Core's count statement alone", stopped: true, bind: func(eng *Engine, nodeID, claimID int64) error {
			bin := int64(7803)
			return eng.db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &bin, 1, 0)
		}},
		{name: "a staged bind from a person's count", bind: func(eng *Engine, nodeID, claimID int64) error {
			_, err := eng.db.BindEmptySlotUnlessDeparted(nodeID, &claimID, 7804, 1, 5)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, _, _, nodeID, _, claimID := betweenBinsSeat(t, false)
			if tc.bound {
				bin := int64(7801)
				testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &bin, 1, 0), "bind 7801")
			}
			before := processes.SeatAsReadOf(runtimeOf(t, eng, nodeID))
			stop(t, eng, nodeID)
			ok, err := processes.SetSeatHold(eng.db.DB, nodeID, 3, "WIDGET-A", before)
			testutil.MustNoErr(t, err, "a write on the seat as read before the stop")
			if ok {
				t.Error("a seat write that read the seat before it stopped landed, want it refused")
			}
			testutil.MustNoErr(t, tc.bind(eng, nodeID, claimID), "the bind")
			if got := runtimeOf(t, eng, nodeID).HoldStopped; got != tc.stopped {
				t.Errorf("hold_stopped after the bind = %t, want %t", got, tc.stopped)
			}
		})
	}
}

// TestHoldStopped_WhatLiftsTheStop — U1, through the engine's doors, on a
// stopped seat with bin 7801 bound at generation 3. Another cart binding
// (ours or a stranger's landing, a hand load) and a person's count (unfenced,
// or fenced as the line's Record Count is) lift the stop. A Core lifecycle
// message never does, though it runs the same statement as a person's
// unfenced count, and a re-landing of the cart bound keeps it.
func TestHoldStopped_WhatLiftsTheStop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		event   func(t *testing.T, eng *Engine, nodeID, claimID int64)
		stopped bool
	}{
		{name: "another cart lands, ours", event: func(t *testing.T, eng *Engine, _, claimID int64) {
			landAt(eng, 7802, orderUUID(t, eng, p2Supply(t, eng, claimID)), 1<<40)
		}},
		{name: "another cart lands, a stranger's", event: func(_ *testing.T, eng *Engine, _, _ int64) {
			landAt(eng, 7803, "", 1<<40)
		}},
		{name: "a hand load at the station", event: func(t *testing.T, eng *Engine, nodeID, claimID int64) {
			node, err := eng.db.GetProcessNode(nodeID)
			testutil.MustNoErr(t, err, "read node")
			bin := int64(7804)
			eng.seatManuallyLoadedBin(node, &claimID, &bin, 1, 0, "WIDGET-A")
		}},
		{name: "a person's unfenced count", event: func(_ *testing.T, eng *Engine, _, _ int64) {
			eng.HandleUOPAdjustment(protocol.UOPAdjustment{BinID: 7801, CoreNodeName: "PRODUCE-NODE",
				NewRemaining: 50, Epoch: 3, Actor: "ui"})
		}},
		{name: "a person's fenced count (Record Count's fencedCount)", event: func(_ *testing.T, eng *Engine, _, _ int64) {
			eng.HandleUOPAdjustment(fenced(eng, 7801, "PRODUCE-NODE", 50, 0, 1))
		}},
		{name: "a Core lifecycle new generation of the cart bound", stopped: true, event: func(_ *testing.T, eng *Engine, _, _ int64) {
			eng.HandleUOPAdjustment(protocol.UOPAdjustment{BinID: 7801, CoreNodeName: "PRODUCE-NODE",
				NewRemaining: 0, Epoch: 4, Actor: protocol.ActorCoreLifecycle})
		}},
		{name: "a re-landing of the cart bound", stopped: true, event: func(_ *testing.T, eng *Engine, _, _ int64) {
			landAt(eng, 7801, "", 1<<40)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, _, _, nodeID, _, claimID := landSeat(t, 0)
			seq := int64(1 << 39)
			_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: 7801, Epoch: 3, Count: betweenBinsPack, PlacedSeq: &seq})
			testutil.MustNoErr(t, err, "bin 7801 bound at generation 3")
			testutil.MustNoErr(t, processes.StopSeatHold(eng.db.DB, nodeID), "the seat stopped")

			tc.event(t, eng, nodeID, claimID)
			rt := runtimeOf(t, eng, nodeID)
			if rt.ActiveBinID == nil {
				t.Fatal("no cart bound after the event")
			}
			if rt.HoldStopped != tc.stopped {
				t.Errorf("stopped after the event = %t, want %t", rt.HoldStopped, tc.stopped)
			}
		})
	}
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
			sm := &strandedMonitor{eng: eng}
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

// TestThrowAway_TheSweepLeavesAStoppedSeatsStretchOpen — D6: one row per
// stretch. A full strict cart is thrown away past one pack and the seat
// stops; the strokes beside the cart join the stretch, and a scan finding
// the seat bound does not send it (it used to end a stretch at every scan of
// a bound seat, which beside a stopped full cart is a row per scan). The
// next cart's landing ends it as one row of all of them.
func TestThrowAway_TheSweepLeavesAStoppedSeatsStretchOpen(t *testing.T) {
	t.Parallel()
	eng, _, processID, nodeID, styleID := strictSeat(t, strictPack, strictPack, "WIDGET-A")
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: eng.db})
	sm := &strandedMonitor{eng: eng}
	strokes(eng, processID, styleID, 4)
	sm.tick(time.Now())
	strokes(eng, processID, styleID, 3)
	sm.tick(time.Now())
	holdCapRow := protocol.HoldDropped{Reason: protocol.HoldDropHoldCap, Qty: strictPack, PayloadCode: "WIDGET-A",
		Node: "PRODUCE-NODE", CalledBy: "edge:tick"}
	assertDrops(t, eng.db, holdCapRow)

	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "RELEASE")
	landAt(eng, 7870, "", 1<<40)
	assertDrops(t, eng.db, holdCapRow, protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 7,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:landing"})
}

// TestThrowAway_AStoppedSeatIsLoggedOnce — the stop is noted once, not per
// stroke: a full strict cart thrown away past one pack, then ten more strokes
// beside it, logs one "holds nothing until another cart binds here" line (the line
// runs a stroke every ~22 s for hours behind a stuck supply). Not parallel: it
// swaps the process logger.
func TestThrowAway_AStoppedSeatIsLoggedOnce(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prev) })

	eng, _, processID, nodeID, styleID := strictSeat(t, strictPack, strictPack, "WIDGET-A")
	strokes(eng, processID, styleID, 11)
	if !runtimeOf(t, eng, nodeID).HoldStopped {
		t.Fatal("fixture: the seat did not stop")
	}
	mu.Lock()
	n := strings.Count(buf.String(), "holds nothing until another cart binds here or a person counts")
	mu.Unlock()
	if n != 1 {
		t.Errorf("the stopped seat logged %d lines over 10 strokes, want 1", n)
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// ── D4: the hold lands only on a cart an order of this Edge's delivered ──

// landSeat is betweenBinsSeat with a sink that writes (binds land) and a
// standing hold of held of WIDGET-A on the unbound seat.
func landSeat(t *testing.T, held int) (eng *Engine, sink *fakeDeltaSink, processID, nodeID, styleID, claimID int64) {
	t.Helper()
	eng, _, processID, nodeID, styleID, claimID = betweenBinsSeat(t, false)
	sink = &fakeDeltaSink{db: eng.db}
	eng.SetInventoryDeltaSink(sink)
	if held != 0 {
		testutil.MustNoErr(t, processes.AddPendingUOPDelta(eng.db.DB, nodeID, held, "WIDGET-A"), "the standing hold")
	}
	return eng, sink, processID, nodeID, styleID, claimID
}

// landAt announces bin landed at the fixture's node by order uuid.
func landAt(eng *Engine, bin int64, uuid string, seq int64) {
	eng.HandleBinLanded(protocol.BinLanded{BinID: bin, CoreNodeName: "PRODUCE-NODE", Epoch: 1,
		OrderUUID: uuid, PlacedSeq: seq})
}

// orderUUID is the uuid of the fixture's order id.
func orderUUID(t *testing.T, eng *Engine, id int64) string {
	t.Helper()
	o, err := eng.db.GetOrder(id)
	testutil.MustNoErr(t, err, "read the order")
	return o.UUID
}

// TestLanding_TheHoldLandsOnlyOnACartAnOrderOfOursDelivered — E4's matrix. A
// seat stands with a hold of 5 and a cart binds. The supply leg placing here
// (an order this Edge holds, RetrieveEmpty, which orderedPart would call "")
// is ours: the hold stays and replays onto the cart. A hand landing, a landing
// naming an order this Edge does not have, one naming an order that delivers
// elsewhere, and a hand load are not: the bind drops the hold in its own
// transaction with one no_carrier row naming the trigger. A re-landing of the
// bin already bound is left alone (H.4).
func TestLanding_TheHoldLandsOnlyOnACartAnOrderOfOursDelivered(t *testing.T) {
	t.Parallel()
	const held = 5
	dropped := func(by string) []protocol.HoldDropped {
		return []protocol.HoldDropped{{Reason: protocol.HoldDropNoCarrier, Qty: held, PayloadCode: "WIDGET-A",
			Node: "PRODUCE-NODE", CalledBy: by}}
	}
	for _, tc := range []struct {
		name  string
		bind  func(t *testing.T, eng *Engine, nodeID, claimID int64)
		kept  bool
		drops []protocol.HoldDropped
	}{
		{name: "the supply leg placing here", kept: true, bind: func(t *testing.T, eng *Engine, _, claimID int64) {
			landAt(eng, 7820, orderUUID(t, eng, p2Supply(t, eng, claimID)), 1<<40)
		}},
		{name: "a hand landing", drops: dropped("edge:landing"), bind: func(t *testing.T, eng *Engine, _, _ int64) {
			landAt(eng, 7821, "", 1<<40)
		}},
		{name: "an order this Edge does not hold", drops: dropped("edge:landing"), bind: func(t *testing.T, eng *Engine, _, _ int64) {
			landAt(eng, 7822, "core-recovery-set-down", 1<<40)
		}},
		{name: "an order that delivers elsewhere", drops: dropped("edge:landing"), bind: func(t *testing.T, eng *Engine, _, _ int64) {
			landAt(eng, 7823, orderUUID(t, eng, p2Order(t, eng.db, protocol.StatusInTransit, "ELSEWHERE", nil, false)), 1<<40)
		}},
		{name: "a re-landing of the bin bound", kept: true, bind: func(t *testing.T, eng *Engine, nodeID, _ int64) {
			seq := int64(1 << 39)
			_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: 7824, Epoch: 1, PlacedSeq: &seq})
			testutil.MustNoErr(t, err, "bin 7824 bound")
			landAt(eng, 7824, "", 1<<40)
		}},
		{name: "a hand load", drops: dropped("edge:manual-load"), bind: func(t *testing.T, eng *Engine, nodeID, claimID int64) {
			node, err := eng.db.GetProcessNode(nodeID)
			testutil.MustNoErr(t, err, "read node")
			bin := int64(7825)
			eng.seatManuallyLoadedBin(node, &claimID, &bin, 1, 0, "WIDGET-A")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, _, _, nodeID, _, claimID := landSeat(t, held)
			tc.bind(t, eng, nodeID, claimID)
			rt := runtimeOf(t, eng, nodeID)
			if rt.ActiveBinID == nil {
				t.Fatal("the cart did not bind")
			}
			want := int64(0)
			if tc.kept {
				want = held
			}
			if rt.PendingUOPDelta != want {
				t.Errorf("hold after the bind = %d, want %d", rt.PendingUOPDelta, want)
			}
			assertDrops(t, eng.db, tc.drops...)
		})
	}
}

// TestLanding_ARefusedBindWritesNoRow — a stranger's landing that the seat
// refuses (a placement stamp older than the one it last bound) binds nothing,
// and the hold drop and its row ride the bind's transaction, so neither is
// written: the hold of 5 stands and no row is queued.
func TestLanding_ARefusedBindWritesNoRow(t *testing.T) {
	t.Parallel()
	eng, _, _, nodeID, _, _ := landSeat(t, 0)
	seq := int64(1 << 41)
	_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: 7860, Epoch: 1, PlacedSeq: &seq})
	testutil.MustNoErr(t, err, "bin 7860 lands at a newer stamp")
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "and leaves")
	testutil.MustNoErr(t, processes.AddPendingUOPDelta(eng.db.DB, nodeID, 5, "WIDGET-A"), "the standing hold")

	landAt(eng, 7861, "", 1<<40)
	rt := runtimeOf(t, eng, nodeID)
	if rt.ActiveBinID != nil || rt.PendingUOPDelta != 5 {
		t.Errorf("seat = bin %v hold %d, want unbound with the hold of 5: the refused bind writes nothing", rt.ActiveBinID, rt.PendingUOPDelta)
	}
	assertDrops(t, eng.db)
}

// TestLanding_TheNormalSwapIsUnchanged — E9.2 (D7): a full strict cart with a
// pile of 6 beside it is released (the release's seat write is carrierLeft;
// the hold is not touched), 4 strokes come in the gap, and the supply leg
// lands: an order this Edge holds, RetrieveEmpty, placing here. The first
// stroke after it charges 11 onto the new cart, and no row is written.
func TestLanding_TheNormalSwapIsUnchanged(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, claimID := strictSeatOn(t, testEngineDB(t), betweenBinsPack, betweenBinsPack, 6, "WIDGET-A", true)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: eng.db})
	supply := p2Supply(t, eng, claimID)
	_, err := eng.db.DB.Exec(`UPDATE orders SET retrieve_empty=1 WHERE id=?`, supply)
	testutil.MustNoErr(t, err, "the supply leg brings an empty")
	if o, err := eng.db.GetOrder(supply); err != nil || !o.RetrieveEmpty {
		t.Fatalf("fixture: the supply leg is not a RetrieveEmpty order (%v)", err)
	}
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "RELEASE")
	strokes(eng, processID, styleID, 4)
	assertSeat(t, eng, nodeID, 0, 10, "WIDGET-A")

	landAt(eng, 7830, orderUUID(t, eng, supply), 1<<40)
	eng.SetInventoryDeltaSink(sink)
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 11, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 11 {
		t.Errorf("deltas = %+v, want one produce_tick of 11", sink.binCalls)
	}
	assertDrops(t, eng.db)
}

// TestLanding_ALateRequestIsAProperty — E9.3, over a range rather than a point:
// a pile of b beside the full cart, the cart released, g strokes in the gap,
// then the supply's cart lands. Within one pack (b + g <= 132) the new cart
// takes b + g with its first stroke, up to its pack (at b + g = 132 the first
// stroke is held beside it), with no row; past it there is exactly one
// hold_cap row of the pile, and the new cart starts at 0 with counting resumed.
func TestLanding_ALateRequestIsAProperty(t *testing.T) {
	t.Parallel()
	for _, b := range []int{0, 55, 115, 131, 132} {
		for _, g := range []int{1, 14, 132} {
			t.Run(fmt.Sprintf("b%d_g%d", b, g), func(t *testing.T) {
				t.Parallel()
				eng, sink, processID, nodeID, styleID, claimID := strictSeatOn(t, testEngineDB(t), betweenBinsPack, betweenBinsPack, b, "WIDGET-A", true)
				eng.SetInventoryDeltaSink(&fakeDeltaSink{db: eng.db})
				testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "RELEASE")
				strokes(eng, processID, styleID, g)
				supply := p2Supply(t, eng, claimID)
				landAt(eng, 7840, orderUUID(t, eng, supply), 1<<40)
				eng.SetInventoryDeltaSink(sink)
				charSPTick(eng, processID, styleID, 1)

				if b+g <= betweenBinsPack {
					count := min(b+g+1, betweenBinsPack)
					holdPart := ""
					if b+g+1 > count {
						holdPart = "WIDGET-A"
					}
					assertSeat(t, eng, nodeID, count, int64(b+g+1-count), holdPart)
					assertDrops(t, eng.db)
					return
				}
				rt := runtimeOf(t, eng, nodeID)
				if rt.RemainingUOPCached != 1 || rt.PendingUOPDelta != 0 || rt.HoldStopped {
					t.Errorf("new cart = count %d hold %d stopped %t, want 1 / 0 / counting", rt.RemainingUOPCached,
						rt.PendingUOPDelta, rt.HoldStopped)
				}
				var capRows []protocol.HoldDropped
				for _, d := range holdDrops(t, eng.db) {
					if d.Reason == protocol.HoldDropHoldCap {
						capRows = append(capRows, d)
					}
				}
				if len(capRows) != 1 || capRows[0].Qty <= 0 || capRows[0].Qty > betweenBinsPack {
					t.Errorf("hold_cap rows = %+v, want exactly one of the pile (at most one pack)", capRows)
				}
			})
		}
	}
}

// ── D5: a person's count wins ──

// TestRecount_APersonsCountClearsTheHold — a full strict cart (132) with 40
// set aside beside it. A person's count of 67, fenced by Core or not, writes
// 67 and clears the hold in the same write, with one no_carrier row by
// edge:recount; the next stroke makes it 68, not 132 (SPR ALN_011,
// 2026-11-12: the recount of 67 was undone 4 s later by the replay). A Core
// lifecycle announcement of the same cart never touches the hold.
func TestRecount_APersonsCountClearsTheHold(t *testing.T) {
	t.Parallel()
	const held = 40
	for _, tc := range []struct {
		name   string
		adj    func(eng *Engine) protocol.UOPAdjustment
		person bool
	}{
		{name: "the bins page, fenced", person: true, adj: func(eng *Engine) protocol.UOPAdjustment {
			return fenced(eng, 7301, "PRODUCE-NODE", 67, 0, 1)
		}},
		{name: "a person's count, unfenced", person: true, adj: func(*Engine) protocol.UOPAdjustment {
			return protocol.UOPAdjustment{BinID: 7301, CoreNodeName: "PRODUCE-NODE", NewRemaining: 67, Epoch: 3, Actor: "ui"}
		}},
		{name: "a Core lifecycle announcement", adj: func(*Engine) protocol.UOPAdjustment {
			return protocol.UOPAdjustment{BinID: 7301, CoreNodeName: "PRODUCE-NODE", NewRemaining: 67, Epoch: 4,
				Actor: protocol.ActorCoreLifecycle}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, _, processID, nodeID, styleID, claimID := strictSeatOn(t, testEngineDB(t), betweenBinsPack, betweenBinsPack, held, "WIDGET-A", true)
			bin := int64(7301)
			testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &bin, 3, betweenBinsPack), "generation 3")
			eng.SetInventoryDeltaSink(&fakeDeltaSink{db: eng.db})

			eng.HandleUOPAdjustment(tc.adj(eng))
			if !tc.person {
				assertSeat(t, eng, nodeID, 67, held, "WIDGET-A")
				assertDrops(t, eng.db)
				return
			}
			assertSeat(t, eng, nodeID, 67, 0, "")
			recount := protocol.HoldDropped{Reason: protocol.HoldDropNoCarrier, Qty: held, PayloadCode: "WIDGET-A",
				Node: "PRODUCE-NODE", CalledBy: "edge:recount"}
			assertDrops(t, eng.db, recount)
			charSPTick(eng, processID, styleID, 1)
			assertSeat(t, eng, nodeID, 68, 0, "")
			assertDrops(t, eng.db, recount)
		})
	}
}

// TestRecount_APersonsCountBindsOverAPile — U2: a person's count of 12 for
// the cart staged at an unbound seat holding a pile of 40 (and stopped) binds
// the cart at 12 and, in the bind's transaction, drops the pile with one
// no_carrier row of 40 by edge:recount and lifts the stop. It used to be
// refused, "not binding over them", and never landed. Controls: Core's own
// lifecycle announcement of the same cart is still refused and keeps the pile
// (as TestProduceFinalizeAnnouncement_DoesNotBindADepartedCarrierOverHeldTicks
// pins), and a late count for the cart that left at a newer generation binds
// nothing and writes no row.
func TestRecount_APersonsCountBindsOverAPile(t *testing.T) {
	t.Parallel()
	const pile = 40
	for _, tc := range []struct {
		name  string
		adj   protocol.UOPAdjustment
		left  bool // bin 7901 left this seat at generation 5 first
		binds bool
	}{
		{name: "a person's count", binds: true,
			adj: protocol.UOPAdjustment{BinID: 7901, CoreNodeName: "PRODUCE-NODE", NewRemaining: 12, Epoch: 1, Actor: "ui"}},
		{name: "Core's lifecycle announcement: refused",
			adj: protocol.UOPAdjustment{BinID: 7901, CoreNodeName: "PRODUCE-NODE", NewRemaining: 12, Epoch: 1, Actor: protocol.ActorCoreLifecycle}},
		{name: "a late count for the cart that left at a newer generation: refused", left: true,
			adj: protocol.UOPAdjustment{BinID: 7901, CoreNodeName: "PRODUCE-NODE", NewRemaining: 12, Epoch: 3, Actor: "ui"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, _, _, nodeID, _, _ := landSeat(t, 0)
			if tc.left {
				seq := int64(1 << 39)
				_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: 7901, Epoch: 5, PlacedSeq: &seq})
				testutil.MustNoErr(t, err, "bin 7901 at generation 5")
				testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "and it leaves")
			}
			testutil.MustNoErr(t, processes.AddPendingUOPDelta(eng.db.DB, nodeID, pile, "WIDGET-A"), "the pile")
			testutil.MustNoErr(t, processes.StopSeatHold(eng.db.DB, nodeID), "the seat stopped")

			eng.HandleUOPAdjustment(tc.adj)
			rt := runtimeOf(t, eng, nodeID)
			if !tc.binds {
				if rt.ActiveBinID != nil || rt.PendingUOPDelta != pile || !rt.HoldStopped {
					t.Errorf("seat = bin %v pile %d stopped %t, want unbound, the pile of %d kept, still stopped",
						rt.ActiveBinID, rt.PendingUOPDelta, rt.HoldStopped, pile)
				}
				assertDrops(t, eng.db)
				return
			}
			if rt.ActiveBinID == nil || *rt.ActiveBinID != 7901 || rt.RemainingUOPCached != 12 {
				t.Errorf("seat = bin %v count %d, want bin 7901 bound at 12", rt.ActiveBinID, rt.RemainingUOPCached)
			}
			if rt.PendingUOPDelta != 0 || rt.HoldStopped {
				t.Errorf("seat pile %d stopped %t, want the pile dropped and the stop lifted", rt.PendingUOPDelta, rt.HoldStopped)
			}
			assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropNoCarrier, Qty: pile, PayloadCode: "WIDGET-A",
				Node: "PRODUCE-NODE", CalledBy: "edge:recount"})
		})
	}
}

// ── D6: one row per no-cart stretch, and a restart cannot lose one ──

// TestShutdown_AnOpenNoCartStretchIsWritten — strokes on an unbound seat with
// nothing coming open a no-cart stretch, which lives in memory until a write
// ends it. Stop writes it: exactly one no_bin row of the stretch's 7 strokes,
// by edge:shutdown; a second Stop writes nothing more.
func TestShutdown_AnOpenNoCartStretchIsWritten(t *testing.T) {
	t.Parallel()
	eng, _, processID, _, styleID, _ := p2Seat(t, 0, "")
	for _, d := range []int64{1, 2, 4} {
		charSPTick(eng, processID, styleID, d)
	}
	assertDrops(t, eng.db)

	eng.Stop()
	shutdown := protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 7, PayloadCode: "WIDGET-A",
		Node: "PRODUCE-NODE", CalledBy: "edge:shutdown"}
	assertDrops(t, eng.db, shutdown)
	eng.Stop()
	assertDrops(t, eng.db, shutdown)
}

// ── E7: a departure only when a carrier left ──

// TestCarrierLeft_AnEmptySeatRecordsNoDeparture — a changeover cancel's reseat
// that finds no carrier at the node (applyReseat) clears the seat through the
// carrier-left verb. On a seat with a carrier bound it records the departure,
// as before; on a seat already empty it records none, so "between bins" is not
// re-armed on a departure that never happened. The pointer and count are
// cleared either way.
func TestCarrierLeft_AnEmptySeatRecordsNoDeparture(t *testing.T) {
	t.Parallel()
	reseat := func(t *testing.T, eng *Engine) {
		t.Helper()
		body, err := json.Marshal(protocol.ReseatAnswer{Node: "PRODUCE-NODE"})
		testutil.MustNoErr(t, err, "the answer")
		applyReseat(eng, &protocol.EdgeAnswer{Status: protocol.AnswerOK, Body: body})
	}
	for _, bound := range []bool{true, false} {
		eng, _, _, nodeID, _ := landSeatBound(t, bound)
		reseat(t, eng)
		rt := runtimeOf(t, eng, nodeID)
		if rt.ActiveBinID != nil || rt.RemainingUOPCached != 0 {
			t.Errorf("bound %t: seat after the reseat = bin %v count %d, want cleared", bound, rt.ActiveBinID, rt.RemainingUOPCached)
		}
		assertDeparted(t, eng, nodeID, bound)
	}
}

// landSeatBound is landSeat with no hold and, when bound, bin 7850 bound at 40
// by its landing; unbound, the seat's last word is a person's (known empty).
func landSeatBound(t *testing.T, bound bool) (eng *Engine, processID, styleID, nodeID, claimID int64) {
	t.Helper()
	eng, _, processID, nodeID, styleID, claimID = landSeat(t, 0)
	if bound {
		seq := int64(1 << 40)
		_, _, err := eng.db.BindLandingAt(nodeID, processes.LandingBind{BinID: 7850, Epoch: 1, Count: 40, PlacedSeq: &seq})
		testutil.MustNoErr(t, err, "bin 7850 lands")
	}
	eng.recordLinesideCarrier(nodeID, "PRODUCE-NODE", domain.KnownCarrier(""), domain.CarrierFromOperator)
	return eng, processID, styleID, nodeID, claimID
}

// ── E9.1: the ALN_011 night, replayed ──

// TestALN011_TheNightReplayed drives SPR ALN_011's night of 2026-11-11/12
// through the engine at delta 1, from the counter readings in the incident's
// EVIDENCE-plant-checks.md §P5: a strict produce seat, pack 132, part 74575.
// CARRIER-0011 (bin 23) is full at 528 (23:33:03), the hold beside it reaches
// 132 at 660 (00:27:50), RELEASE comes at 665 (00:29:52), the line runs to
// 1327 (06:50:14) behind a supply leg that stays live until it is cancelled
// at 07:00:31, runs again to 1459 (08:12:47), and makes 1460 at 09:33:47 on
// bin 23, recorded at the line by hand as empty.
//
// Exactly two rows: one hold_cap of 132 at the stroke that would take the
// pile past one pack (661, 00:28:12), and one no_bin of 799 closed at bin 35's
// hand landing; together the 931 strokes made after the cart filled. The hold
// never passes 132, bin 23 reads 1 after 09:33:47 and asks for no swap, and a
// person's count of 67 stays: the next stroke makes 68. On the night the sweep
// dropped 794, bin 23 read 132 and fired a swap, and the recount was undone 4
// s later.
func TestALN011_TheNightReplayed(t *testing.T) {
	t.Parallel()
	const pack = 132
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedProduceNode(t, db, protocol.SwapModeTwoRobot)
	_, err := db.DB.Exec(`UPDATE payload_catalog SET code='74575', uop_capacity=? WHERE code='WIDGET-A'`, pack)
	testutil.MustNoErr(t, err, "part 74575, pack 132")
	_, err = db.DB.Exec(`UPDATE style_node_claims SET payload_code='74575', auto_request_payload='74575', strict_pack=1 WHERE id=?`, claimID)
	testutil.MustNoErr(t, err, "the strict claim")
	bin23 := int64(23)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &bin23, 1, pack), "CARRIER-0011 full at 528")
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	claim := p2Claim(t, eng, claimID)
	if claim.UOPCapacity != pack || !claim.StrictPackApplies() {
		t.Fatalf("fixture: claim pack %d strict %t, want %d strict", claim.UOPCapacity, claim.StrictPackApplies(), pack)
	}
	supply := p2Supply(t, eng, claimID) // live from 23:25, its release facts placing ALN_011
	uuidOf := func(id int64) string {
		o, err := db.GetOrder(id)
		testutil.MustNoErr(t, err, "read order")
		return o.UUID
	}
	evacLeg := func() int64 {
		_, evac := BuildTwoRobotSwapSteps(claim)
		return p2Order(t, db, protocol.StatusInTransit, "", evac, false)
	}

	counter, maxHold := 528, int64(0)
	strokeTo := func(to int) {
		t.Helper()
		for ; counter < to; counter++ {
			charSPTick(eng, processID, styleID, 1)
			if h := runtimeOf(t, eng, nodeID).PendingUOPDelta; h > maxHold {
				maxHold = h
			}
		}
	}
	holdCapRow := protocol.HoldDropped{Reason: protocol.HoldDropHoldCap, Qty: pack, PayloadCode: "74575",
		Node: "PRODUCE-NODE", CalledBy: "edge:tick"}

	strokeTo(660) // 00:27:50: the hold beside the full cart reaches one pack
	assertSeat(t, eng, nodeID, pack, pack, "74575")
	strokeTo(661) // 00:28:12: the stroke that would pass it
	assertDrops(t, db, holdCapRow)
	strokeTo(665) // 00:29:39, the last read before the release

	// 00:29:52 RELEASE. Its seat write is the carrier-left verb
	// (release_commit.go); the hold is not touched (D7).
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "PRODUCE-NODE"), "RELEASE")
	// 00:30:06 the evac's pickup of bin 23, which the seat no longer holds.
	eng.HandleBinPickedUpMessage(protocol.BinPickedUp{OrderUUID: uuidOf(evacLeg()), BinID: 23, Location: "PRODUCE-NODE"})
	strokeTo(1327) // 06:50:14; the line stops

	// 07:00:31 the supply is cancelled on the stuck robot, and the cancel's
	// reseat finds no carrier at the node.
	_, err = db.DB.Exec(`UPDATE orders SET status=? WHERE id=?`, string(protocol.StatusCancelled), supply)
	testutil.MustNoErr(t, err, "cancel the supply")
	body, err := json.Marshal(protocol.ReseatAnswer{Node: "PRODUCE-NODE"})
	testutil.MustNoErr(t, err, "the reseat answer")
	applyReseat(eng, &protocol.EdgeAnswer{Status: protocol.AnswerOK, Body: body})
	// 07:00:51 the sweep.
	(&strandedMonitor{eng: eng}).tick(time.Now())
	assertDrops(t, db, holdCapRow)
	strokeTo(1459) // 07:20:52-08:12:47; the line stops

	// 08:49:56 bin 35 placed at the line by hand, and lifted.
	eng.HandleBinLanded(protocol.BinLanded{BinID: 35, CoreNodeName: "PRODUCE-NODE", Count: 105, Epoch: 1, PlacedSeq: 1 << 40})
	noBinRow := protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 799, PayloadCode: "74575",
		Node: "PRODUCE-NODE", CalledBy: "edge:landing"}
	assertDrops(t, db, holdCapRow, noBinRow)
	eng.HandleBinPickedUpMessage(protocol.BinPickedUp{OrderUUID: uuidOf(evacLeg()), BinID: 35, Location: "PRODUCE-NODE"})

	// 09:32:29 bin 23 recorded at the line by hand, empty; 09:33:47 one stroke.
	eng.HandleBinLanded(protocol.BinLanded{BinID: 23, CoreNodeName: "PRODUCE-NODE", Count: 0, Epoch: 2, PlacedSeq: 1<<40 + 1})
	strokeTo(1460)
	assertSeat(t, eng, nodeID, 1, 0, "")
	before := countOrders(t, db)
	eng.reconcileDemand()
	if after := countOrders(t, db); after != before {
		t.Errorf("orders %d -> %d after the 09:33:47 stroke: a produce swap was requested for a cart reading 1", before, after)
	}

	// 10:07:04 a person's count of 67; one stroke.
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{BinID: 23, CoreNodeName: "PRODUCE-NODE", NewRemaining: 67, Epoch: 2, Actor: "ui"})
	strokeTo(1461)
	assertSeat(t, eng, nodeID, 68, 0, "")

	assertDrops(t, db, holdCapRow, noBinRow)
	if holdCapRow.Qty+noBinRow.Qty != 1459-528 {
		t.Errorf("the rows hold %d strokes, want the %d made after the cart filled", holdCapRow.Qty+noBinRow.Qty, 1459-528)
	}
	if maxHold > pack {
		t.Errorf("the hold reached %d, want never past one pack (%d)", maxHold, pack)
	}

	// CONTROL: the reconciler run above was able to ask. A full cart with
	// nothing coming asks for its swap in this fixture.
	testutil.MustNoErr(t, db.UpdateProcessNodeUOP(nodeID, pack), "bin 23 full")
	before = countOrders(t, db)
	eng.reconcileDemand()
	if after := countOrders(t, db); after == before {
		t.Error("control: a full cart asked for no swap -- the no-swap check above could not fail")
	}
}
