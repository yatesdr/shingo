package engine

import (
	"fmt"
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
// stops the seat, which sets nothing aside (the no_bin stretch) until a cart
// binds (D2/D3). A seat with no Shingo departure logs, as before.

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

// bindCart binds bin at the seat through a real bind statement (Core's bind,
// SetRuntimeWithBinAndEpoch), which clears the stop flag as every bind does;
// SetProcessNodeActiveBinID is a fixture pointer write and clears nothing.
func bindCart(t *testing.T, eng *Engine, nodeID, bin int64) {
	t.Helper()
	rt := runtimeOf(t, eng, nodeID)
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, rt.ActiveClaimID, &bin, 1, 0), "a cart binds")
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
// read the seat before it was set is refused (SeatAsRead names it), and every
// bind of a cart clears it: a landing of another bin, a hand load, Core's bind
// and a staged bind. A re-landing of the bin already bound keeps it (H.4).
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
		{name: "a hand load or Core's bind", bind: func(eng *Engine, nodeID, claimID int64) error {
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
