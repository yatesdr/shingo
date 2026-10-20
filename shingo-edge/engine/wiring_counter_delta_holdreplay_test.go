package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/release"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// TestHoldAndReplay_BinUOPDeltaLumpsAcrossBinSwapGap is the gating correctness
// test for slice 5's data-source decision (plan §8 #13, §12). It proves the
// DESTRUCTIVE half of the architectural premise: the inventory BinUOPDelta
// stream — what the production-heartbeat dashboard would have to read if it
// sourced per-part cadence from the inventory channel — destroys per-tick
// timing across a finalize→new-empty-bin gap.
//
// Mechanism (wiring_counter_delta.go::handleProduceTick → applyHoldAndReplay):
// produce ticks that fire while no bin is bound (active_bin_id == nil) are HELD
// in pending_uop_delta and emit NO BinUOPDelta (binAtNode returns binID 0, which
// the mutator skips). When the next empty bin binds, the first tick replays the
// held total LUMPED onto it as a single delta. So three distinct part-fire
// events collapse into one event attributed to the rebind moment — and the
// BinUOPDelta stream carries no per-tick timestamp at all.
//
// The PRESERVING half — production.tick keeps one event per tick with its own
// RecordedAt across the same gap — is proven in
// plc/manager_production_tick_test.go. production.tick is emitted in the PLC
// manager UPSTREAM of this hold-and-replay, which is precisely why it is immune
// to the lumping shown here. Together the two tests are the code substitute for
// the unrun live "tap bin_uop_delta across a changeover" spike (§8 #13).
//
// If this test ever fails (BinUOPDelta did NOT lump), slice 5 would not need a
// separate production.tick channel at all — so this is the test that justifies
// the whole channel.
//
// Modeling note: the gap is created by clearing active_bin_id directly, which
// is the durable post-pickup runtime state that FinalizeProduceNode + the robot
// pickup produce. We model it directly rather than calling FinalizeProduceNode
// so the test isolates hold-and-replay from FinalizeProduceNode's dispatch and
// capture-to-lineside side effects — the same approach as the canonical
// consume-side TestRuntimeBinding_PLCTicksHoldWhenNoBinThenReplayOntoNextBin.
func TestHoldAndReplay_BinUOPDeltaLumpsAcrossBinSwapGap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedProduceNode(t, db, protocol.SwapModeSimple)

	// Bin A physically at the slot. Start the cache low (10) so the produce
	// node's AutoReorder capacity relief (capacity 100) never fires mid-test
	// and calls FinalizeProduceNode under us.
	const binA int64 = 7001
	const binB int64 = 7002
	bA := binA
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bA, 10), "bind bin A")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)

	tick := func() {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: 1,
		}})
	}

	// ── 3 ticks while bin A is bound: each emits its own BinUOPDelta ──
	tick()
	tick()
	tick()
	if len(sink.binCalls) != 3 {
		t.Fatalf("after 3 bound ticks: binCalls=%d, want 3 (one BinUOPDelta per tick): %+v",
			len(sink.binCalls), sink.binCalls)
	}
	for i, bc := range sink.binCalls {
		if bc.BinID != binA || bc.Delta != 1 || bc.Reason != protocol.ReasonProduceTick {
			t.Errorf("bound tick %d: got %+v, want {BinID:%d Delta:1 Reason:%s}",
				i, bc, binA, protocol.ReasonProduceTick)
		}
	}

	// ── finalize/pickup: bin A leaves the slot, new bin not yet delivered ──
	// The order bringing bin B is live, so the gap's ticks are held for it (an
	// unbound produce seat holds only while a carrier is coming).
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "clear active bin (open the gap)")
	p2Supply(t, eng, claimID)

	// ── 3 ticks during the gap: HELD on the runtime row, emit NOTHING ──
	tick()
	tick()
	tick()
	if len(sink.binCalls) != 3 {
		t.Fatalf("after 3 gap ticks: binCalls=%d, want still 3 — gap ticks must NOT emit a BinUOPDelta (binID 0 is skipped, the count is held): %+v",
			len(sink.binCalls), sink.binCalls)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime after gap")
	if rt.PendingUOPDelta != 3 {
		t.Fatalf("PendingUOPDelta=%d after 3 gap ticks, want 3 (each held durably on the runtime row)", rt.PendingUOPDelta)
	}
	if rt.RemainingUOPCached != 13 {
		t.Errorf("RemainingUOPCached=%d after gap, want 13 (held ticks must not touch the cache while unbound)", rt.RemainingUOPCached)
	}

	// ── new empty bin B binds; the first tick after rebind replays the lump ──
	bB := binB
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bB), "bind bin B")
	tick()

	if len(sink.binCalls) != 4 {
		t.Fatalf("after rebind + 1 tick: binCalls=%d, want 4 — the 3 held gap ticks collapse into ONE lumped delta: %+v",
			len(sink.binCalls), sink.binCalls)
	}
	lumped := sink.binCalls[3]
	if lumped.BinID != binB {
		t.Errorf("lumped delta BinID=%d, want %d (attributed to the rebind bin, NOT to anything live during the gap)",
			lumped.BinID, binB)
	}
	if lumped.Delta != 4 {
		t.Errorf("lumped delta=%d, want 4 — three held gap ticks + the one rebind tick, replayed as a SINGLE event; the three gap ticks' individual arrival times are gone",
			lumped.Delta)
	}
	if lumped.Reason != protocol.ReasonProduceTick {
		t.Errorf("lumped delta reason=%q, want %q", lumped.Reason, protocol.ReasonProduceTick)
	}

	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime after replay")
	if rt.PendingUOPDelta != 0 {
		t.Errorf("PendingUOPDelta=%d after replay, want 0 (held delta consumed onto the new bin)", rt.PendingUOPDelta)
	}

	// The destruction, stated plainly: 6 distinct part-fire ticks produced only
	// 4 BinUOPDelta events, and not one of them carries a per-tick timestamp —
	// the 3 gap ticks exist nowhere in the stream as individual events. A
	// dashboard reading BinUOPDelta sees the cell idle for the whole gap and
	// then fire 4 parts instantaneously at rebind. production.tick (see the plc
	// test) instead carries all 6 ticks, each with its own RecordedAt.
}

// A produce node is delivered an EMPTY, which Core reports as a known-empty
// carrier. Every produce tick must still carry the claim's part, or Core's
// first-delta bind never labels the bin (SPR ALN_011 2026-10-08: 81 parts on an
// unlabelled carrier; HK press bins labelled only at release since 09-06).
func TestProduceTick_KnownEmptyCarrierStillCarriesTheClaimsPart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedProduceNode(t, db, protocol.SwapModeSimple)

	const binID int64 = 7101
	b := binID
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &b, 0), "bind empty")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	eng.recordLinesideCarrier(nodeID, "PRODUCE-NODE", domain.KnownCarrier(""), domain.CarrierFromDelivery)

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 1,
	}})

	if len(sink.binCalls) != 1 {
		t.Fatalf("binCalls=%d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	claim, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim")
	if got := sink.binCalls[0].PayloadCode; got != claim.PayloadCode {
		t.Errorf("produce tick payload = %q, want the claim's %q: a blank part on the wire means "+
			"Core never labels the bin it is filling", got, claim.PayloadCode)
	}
}

// ── Strict-pack characterisation (step 1): the produce hold at the base ──
//
// These pin what chargeOrHold does to a produce seat today, so the strict-pack
// lanes flip them deliberately. The pack is the claim's UOPCapacity, which is
// resolved from payload_catalog on every read; seedProduceNode writes the
// catalog row (seedClaimCapacity), so WIDGET-A's pack here is 100.

// charSPProduceSeat seeds the produce node, empties its seat, seeds a hold of
// held parts of the claim's part, and wires an engine with a recording sink.
func charSPProduceSeat(t *testing.T, held int) (eng *Engine, sink *fakeDeltaSink, processID, nodeID, styleID, claimID int64) {
	t.Helper()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID = seedProduceNode(t, db, protocol.SwapModeTwoRobot)
	claim, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim")
	if claim.UOPCapacity != 100 {
		t.Fatalf("fixture: pack = %d, want the catalog's 100", claim.UOPCapacity)
	}
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 0), "empty the seat")
	testutil.MustNoErr(t, processes.AddPendingUOPDelta(db.DB, nodeID, held, claim.PayloadCode), "seed the hold")
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	sink = &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	return eng, sink, processID, nodeID, styleID, claimID
}

func charSPTick(eng *Engine, processID, styleID, delta int64) {
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: delta,
	}})
}

// TestCharSP_UnboundProduceTickWithNoCarrierDropsThePile — a produce seat with
// no bin bound and a standing hold of 3,000 of the claim's part (pack 100)
// takes a tick of 2, with no order placing a carrier at the node (none exists).
// At the base it was held on top, uncapped (3,002). Now the standing pile is
// dropped with one hold_dropped (no_carrier) row of 3,000 and the tick is
// logged, not held: nothing is charged, held or emitted.
func TestCharSP_UnboundProduceTickWithNoCarrierDropsThePile(t *testing.T) {
	t.Parallel()
	const held = 3000
	eng, sink, processID, nodeID, styleID, _ := charSPProduceSeat(t, held)
	var orders int
	testutil.MustNoErr(t, eng.db.DB.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&orders), "count orders")
	if orders != 0 {
		t.Fatalf("fixture: %d orders exist, want none", orders)
	}

	charSPTick(eng, processID, styleID, 2)

	rt := runtimeOf(t, eng, nodeID)
	if rt.PendingUOPDelta != 0 || rt.PendingUOPPayload != "" {
		t.Errorf("hold = %d of %q, want none: the pile is dropped and the tick is not held", rt.PendingUOPDelta, rt.PendingUOPPayload)
	}
	if rt.RemainingUOPCached != 0 || rt.ActiveBinID != nil {
		t.Errorf("seat = bin %v count %d, want unbound at 0: nothing is charged", rt.ActiveBinID, rt.RemainingUOPCached)
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("bin deltas = %+v, want none", sink.binCalls)
	}
	assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropNoCarrier, Qty: held,
		PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
}

// TestCharSP_FirstBoundTickReplaysTheWholeHold — the first tick after a bin of
// the held part binds. Under the pack (strict_pack off) it charges input + held
// in one write (ChargeSeatUOP with clearPending), clears the hold and its part,
// and emits ONE produce_tick of input + held against the bin.
//
// Over the pack the case runs with strict_pack on, and the strict arm answers
// it (it was 3,001 on a pack of 100 at the base, when nothing on the produce
// path read the pack): a hold larger than the pack is the backstop, so the hold
// is offered nothing and dropped with one hold_dropped (backstop) row of 3,000,
// and the bin takes the tick alone, emitting 1.
func TestCharSP_FirstBoundTickReplaysTheWholeHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		held   int
		strict bool
		count  int // the bin's count after the tick
		drop   int64
	}{
		{"hold under the pack", 5, false, 6, 0},
		{"hold over the pack, strict", 3000, true, 1, 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, sink, processID, nodeID, styleID, claimID := charSPProduceSeat(t, tc.held)
			_, err := eng.db.DB.Exec(`UPDATE style_node_claims SET strict_pack=? WHERE id=?`, tc.strict, claimID)
			testutil.MustNoErr(t, err, "set strict_pack")
			const binID int64 = 7201
			b := binID
			testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &b), "bind a bin of the part")

			charSPTick(eng, processID, styleID, 1)

			rt := runtimeOf(t, eng, nodeID)
			if rt.RemainingUOPCached != tc.count {
				t.Errorf("bin count = %d, want %d", rt.RemainingUOPCached, tc.count)
			}
			if rt.PendingUOPDelta != 0 || rt.PendingUOPPayload != "" {
				t.Errorf("hold = %d of %q, want cleared", rt.PendingUOPDelta, rt.PendingUOPPayload)
			}
			if len(sink.binCalls) != 1 {
				t.Fatalf("bin deltas = %+v, want exactly one", sink.binCalls)
			}
			bc := sink.binCalls[0]
			if bc.BinID != binID || bc.Delta != tc.count || bc.Reason != protocol.ReasonProduceTick || bc.PayloadCode != "WIDGET-A" {
				t.Errorf("delta = %+v, want {bin %d, +%d, produce_tick, WIDGET-A}", bc, binID, tc.count)
			}
			drops := holdDrops(t, eng.db)
			switch {
			case tc.drop == 0 && len(drops) != 0:
				t.Errorf("drops = %+v, want none", drops)
			case tc.drop != 0 && (len(drops) != 1 || drops[0].Reason != protocol.HoldDropBackstop ||
				drops[0].Qty != tc.drop || drops[0].PayloadCode != "WIDGET-A"):
				t.Errorf("drops = %+v, want one backstop of %d WIDGET-A", drops, tc.drop)
			}
		})
	}
}

// ── Strict pack: the bound bin is charged only up to its pack ──
//
// The pack is the tick's matched claim's UOPCapacity (catalog-resolved on every
// read), the gate is strictPackApplies, and the split is splitStrict. Unless a
// pin says otherwise the pack is 132, the seat names the claim
// (active_claim_id), and the claim's part is WIDGET-A.

const strictPack = 132

// strictSeatOn seeds the produce node on db with the claim's pack set to pack
// and strict_pack set to strict, binds bin 7301 at count with a hold of held of
// heldPart (no hold when held is 0), and wires an engine with a recording sink.
func strictSeatOn(t *testing.T, db *store.DB, pack, count, held int, heldPart string, strict bool) (eng *Engine, sink *fakeDeltaSink, processID, nodeID, styleID, claimID int64) {
	t.Helper()
	processID, nodeID, styleID, claimID = seedProduceNode(t, db, protocol.SwapModeTwoRobot)
	_, err := db.DB.Exec(`UPDATE payload_catalog SET uop_capacity=? WHERE code='WIDGET-A'`, pack)
	testutil.MustNoErr(t, err, "set the pack")
	_, err = db.DB.Exec(`UPDATE style_node_claims SET strict_pack=? WHERE id=?`, strict, claimID)
	testutil.MustNoErr(t, err, "set strict_pack")
	bin := int64(7301)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, count), "bind the bin")
	if held != 0 {
		testutil.MustNoErr(t, processes.AddPendingUOPDelta(db.DB, nodeID, held, heldPart), "seed the hold")
	}
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	sink = &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	return eng, sink, processID, nodeID, styleID, claimID
}

func strictSeat(t *testing.T, count, held int, heldPart string) (*Engine, *fakeDeltaSink, int64, int64, int64) {
	t.Helper()
	eng, sink, processID, nodeID, styleID, _ := strictSeatOn(t, testEngineDB(t), strictPack, count, held, heldPart, true)
	return eng, sink, processID, nodeID, styleID
}

// holdDrops reads the hold_dropped messages queued in the outbox, in order.
func holdDrops(t *testing.T, db *store.DB) []protocol.HoldDropped {
	t.Helper()
	msgs, err := db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "list outbox")
	var out []protocol.HoldDropped
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectHoldDropped {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode data")
		var d protocol.HoldDropped
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &d), "decode drop")
		out = append(out, d)
	}
	return out
}

// emitted is the sum of the produce_tick deltas the sink recorded, each checked
// to be a produce_tick of the claim's part.
func emitted(t *testing.T, sink *fakeDeltaSink) int {
	t.Helper()
	total := 0
	for _, bc := range sink.binCalls {
		if bc.Reason != protocol.ReasonProduceTick || bc.PayloadCode != "WIDGET-A" {
			t.Errorf("delta %+v, want a produce_tick of WIDGET-A", bc)
		}
		total += bc.Delta
	}
	return total
}

// assertSeat checks the bound count and the hold.
func assertSeat(t *testing.T, eng *Engine, nodeID int64, count int, hold int64, holdPart string) {
	t.Helper()
	rt := runtimeOf(t, eng, nodeID)
	if rt.RemainingUOPCached != count || rt.PendingUOPDelta != hold || rt.PendingUOPPayload != holdPart {
		t.Errorf("seat = count %d, hold %d of %q; want count %d, hold %d of %q",
			rt.RemainingUOPCached, rt.PendingUOPDelta, rt.PendingUOPPayload, count, hold, holdPart)
	}
}

// TestStrictPack_CrossingTickChargesToThePackAndHoldsTheRest: 131 + 2 on a pack
// of 132 charges 1, holds 1 for the next bin of the part, and emits 1 (the
// held part is emitted once, on the next bin's replay, never here).
func TestStrictPack_CrossingTickChargesToThePackAndHoldsTheRest(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := strictSeat(t, 131, 0, "")
	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, 132, 1, "WIDGET-A")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	if d := holdDrops(t, eng.db); len(d) != 0 {
		t.Errorf("drops = %+v, want none", d)
	}
}

// TestStrictPack_AtPackTickHoldsWithoutEmit: a bin at its pack takes nothing;
// the tick is held and nothing is emitted.
func TestStrictPack_AtPackTickHoldsWithoutEmit(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := strictSeat(t, strictPack, 0, "")
	charSPTick(eng, processID, styleID, 3)
	assertSeat(t, eng, nodeID, strictPack, 3, "WIDGET-A")
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none: nothing was charged", sink.binCalls)
	}
}

// TestStrictPack_APersonsCountAboveThePackIsNotWrittenDown: a count already
// past the pack (a person's 140) stands; the tick takes 0 and is held.
func TestStrictPack_APersonsCountAboveThePackIsNotWrittenDown(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := strictSeat(t, 140, 0, "")
	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, 140, 2, "WIDGET-A")
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none", sink.binCalls)
	}
}

// TestStrictPack_ReplayOntoAFreshBinEmitsOnce: a fresh bin (0) with 24 held of
// the part and a tick of 2 takes all 26 in one write, the hold and its part
// clear, and 26 is emitted once.
func TestStrictPack_ReplayOntoAFreshBinEmitsOnce(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := strictSeat(t, 0, 24, "WIDGET-A")
	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, 26, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 26 {
		t.Errorf("deltas = %+v, want one produce_tick of 26", sink.binCalls)
	}
}

// TestStrictPack_PartialReplayKeepsTheRemainderWithItsPart: a bin at 120 with
// 20 held and a tick of 2 takes the 12 that fit; the other 10 stay held, with
// their part, for the next bin.
func TestStrictPack_PartialReplayKeepsTheRemainderWithItsPart(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := strictSeat(t, 120, 20, "WIDGET-A")
	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, strictPack, 10, "WIDGET-A")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 12 {
		t.Errorf("deltas = %+v, want one produce_tick of 12", sink.binCalls)
	}
}

// TestStrictPack_ForeignHoldIsNotRelabelledAndTheRestIsDropped: beside a bound
// bin at 131, a hold of another part is not offered and not touched; the tick
// of 3 charges the 1 that fits, and the 2 that fit nowhere are dropped with one
// foreign_hold row naming the tick's part.
func TestStrictPack_ForeignHoldIsNotRelabelledAndTheRestIsDropped(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := strictSeat(t, 131, 5, "OTHER-PART")
	charSPTick(eng, processID, styleID, 3)
	assertSeat(t, eng, nodeID, strictPack, 5, "OTHER-PART")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	drops := holdDrops(t, eng.db)
	if len(drops) != 1 {
		t.Fatalf("drops = %+v, want exactly one", drops)
	}
	if d := drops[0]; d.Reason != protocol.HoldDropForeignHold || d.Qty != 2 || d.PayloadCode != "WIDGET-A" || d.Node != "PRODUCE-NODE" {
		t.Errorf("drop = %+v, want foreign_hold of 2 WIDGET-A at PRODUCE-NODE", d)
	}
}

// TestStrictPack_HoldCapDropsTheExcessOverOnePack: a bin at its pack with 131
// held takes a tick of 5. Nothing is charged; the hold would be 136, so it
// holds exactly one pack and the 4 over it are dropped with one hold_cap row.
func TestStrictPack_HoldCapDropsTheExcessOverOnePack(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := strictSeat(t, strictPack, 131, "WIDGET-A")
	charSPTick(eng, processID, styleID, 5)
	assertSeat(t, eng, nodeID, strictPack, strictPack, "WIDGET-A")
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none", sink.binCalls)
	}
	drops := holdDrops(t, eng.db)
	if len(drops) != 1 {
		t.Fatalf("drops = %+v, want exactly one", drops)
	}
	if d := drops[0]; d.Reason != protocol.HoldDropHoldCap || d.Qty != 4 || d.PayloadCode != "WIDGET-A" {
		t.Errorf("drop = %+v, want hold_cap of 4 WIDGET-A", d)
	}
}

// TestStrictPack_ACountWrittenBetweenReadAndLockRetriesOnce: the tick read the
// seat at 120; a person's count of 130 lands before it takes the lock. The
// strict write pins the count it read, so it is refused once, the seat is
// re-read once, and the split is made on 130: 2 charged, 3 held, 2 emitted.
func TestStrictPack_ACountWrittenBetweenReadAndLockRetriesOnce(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng, sink, _, nodeID, _, claimID := strictSeatOn(t, db, strictPack, 120, 0, "", true)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	claim, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim")
	stale := runtimeOf(t, eng, nodeID)
	testutil.MustNoErr(t, db.UpdateProcessNodeUOP(nodeID, 130), "a person's count")

	counter.Reset()
	eng.handleProduceTick(node, stale, claim, 5)

	var updates, reads int
	for _, q := range counter.Statements() {
		switch {
		case strings.Contains(q, "UPDATE process_node_runtime_states"):
			updates++
		case strings.Contains(q, "FROM process_node_runtime_states"):
			reads++
		}
	}
	if updates != 2 || reads != 1 {
		t.Errorf("seat statements = %d updates, %d reads; want 2 and 1 (refused, re-read, landed): %q",
			updates, reads, counter.Statements())
	}
	assertSeat(t, eng, nodeID, strictPack, 3, "WIDGET-A")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 2 {
		t.Errorf("deltas = %+v, want one produce_tick of 2", sink.binCalls)
	}
}

// TestStrictPack_TickLedgerOverABinCycleEqualsStrokes: on a pack of 100, 103
// strokes fill bin 1 to the pack and hold 3; the bin leaves; 2 strokes on the
// empty seat are held; bin 2 binds and 1 stroke replays the 5. Bin 1 counts
// 100, bin 2 counts 6, and the produce_tick ledger is 106: every stroke once.
func TestStrictPack_TickLedgerOverABinCycleEqualsStrokes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, sink, processID, nodeID, styleID, claimID := strictSeatOn(t, db, 100, 0, 0, "", true)
	p2Supply(t, eng, claimID) // the order bringing bin 2: the gap's strokes are held for it
	strokes := 0
	stroke := func(n int) {
		for i := 0; i < n; i++ {
			charSPTick(eng, processID, styleID, 1)
			strokes++
		}
	}
	stroke(103)
	assertSeat(t, eng, nodeID, 100, 3, "WIDGET-A")
	bin1 := emitted(t, sink)

	testutil.MustNoErr(t, db.ClearProcessNodeActiveBinAndCount(nodeID), "bin 1 leaves")
	stroke(2)
	b2 := int64(7302)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &b2), "bin 2 binds")
	stroke(1)
	assertSeat(t, eng, nodeID, 6, 0, "")

	if bin1 != 100 {
		t.Errorf("bin 1 ledger = %d, want 100 (the pack)", bin1)
	}
	if total := emitted(t, sink); total != strokes {
		t.Errorf("produce_tick ledger = %d over %d strokes; want equal", total, strokes)
	}
	if d := holdDrops(t, db); len(d) != 0 {
		t.Errorf("drops = %+v, want none", d)
	}
}

// TestStrictPack_InertOffTheGate: with the flag off, with no pack (cap 0), and
// on a consume claim carrying the flag, a tick runs today's statement
// (ChargeSeatUOP: the count moved by a delta on the seat as read, no count
// pinned) with today's answer: the produce count passes the pack.
func TestStrictPack_InertOffTheGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		pack   int
		strict bool
	}{
		{"the flag off", strictPack, false},
		{"no pack", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, counter := testEngineDBCounting(t)
			eng, sink, processID, nodeID, styleID, _ := strictSeatOn(t, db, tc.pack, 131, 0, "", tc.strict)
			counter.Reset()
			charSPTick(eng, processID, styleID, 2)
			assertSeat(t, eng, nodeID, 133, 0, "")
			if len(sink.binCalls) != 1 || emitted(t, sink) != 2 {
				t.Errorf("deltas = %+v, want one produce_tick of 2", sink.binCalls)
			}
			assertTodaysChargeStatement(t, counter.Statements())
		})
	}
	t.Run("a consume claim", func(t *testing.T) {
		t.Parallel()
		db, counter := testEngineDBCounting(t)
		processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
			Prefix: "STRICT-CON", PayloadCode: "SYN-CON-A", UOPCapacity: 100, InitialUOP: 5})
		_, err := db.DB.Exec(`UPDATE style_node_claims SET strict_pack=1 WHERE id=?`, claimID)
		testutil.MustNoErr(t, err, "set strict_pack")
		eng := testEngine(t, db)
		eng.wireEventHandlers()
		counter.Reset()
		charSPTick(eng, processID, styleID, 2)
		if rt := runtimeOf(t, eng, nodeID); rt.RemainingUOPCached != 3 {
			t.Errorf("count = %d, want 3: a consume tick draws 2 as today", rt.RemainingUOPCached)
		}
		assertTodaysChargeStatement(t, counter.Statements())
	})
}

// assertTodaysChargeStatement: the tick's one seat write is ChargeSeatUOP's,
// and nothing pinned the count or capped it.
func assertTodaysChargeStatement(t *testing.T, stmts []string) {
	t.Helper()
	var charges []string
	for _, q := range stmts {
		if strings.Contains(q, "UPDATE process_node_runtime_states") {
			charges = append(charges, q)
		}
	}
	if len(charges) != 1 || !strings.Contains(charges[0], "remaining_uop_cached = remaining_uop_cached + ?") ||
		strings.Contains(charges[0], "remaining_uop_cached=?") || strings.Contains(charges[0], "MIN(") {
		t.Errorf("seat writes = %q, want exactly today's ChargeSeatUOP", charges)
	}
}

// TestStrictPack_InertWhenTheSeatNamesNoClaim: a runtime row whose
// active_claim_id is NULL (a seat never through a changeover, a Clear or a
// Record Count) takes today's uncapped charge under the gate as written:
// strictPackApplies requires the seat to name the tick's claim. STOP S1 is open
// on that conjunct; this pin states today's answer, so a ruling is one line in
// strictPackApplies and this pin's expectation.
func TestStrictPack_InertWhenTheSeatNamesNoClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, sink, processID, nodeID, styleID, _ := strictSeatOn(t, db, strictPack, 131, 0, "", true)
	_, err := db.DB.Exec(`UPDATE process_node_runtime_states SET active_claim_id=NULL WHERE process_node_id=?`, nodeID)
	testutil.MustNoErr(t, err, "clear the seat's claim")
	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, 133, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 2 {
		t.Errorf("deltas = %+v, want one produce_tick of 2", sink.binCalls)
	}
}

// TestStrictPack_ARefusedWriteQueuesNoDrop: the drop row and the seat write it
// describes commit together or not at all. The tick read a bin at 131 beside a
// hold of another part, so its first split charges 1 and drops 2; a person's
// count of 132 lands before the lock, the write is refused, and that attempt's
// row goes with it. The retry splits on 132: 0 charged, 3 dropped, in one row.
func TestStrictPack_ARefusedWriteQueuesNoDrop(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, sink, _, nodeID, _, claimID := strictSeatOn(t, db, strictPack, 131, 5, "OTHER-PART", true)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	claim, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim")
	stale := runtimeOf(t, eng, nodeID)
	testutil.MustNoErr(t, db.UpdateProcessNodeUOP(nodeID, strictPack), "a person's count")

	eng.handleProduceTick(node, stale, claim, 3)

	assertSeat(t, eng, nodeID, strictPack, 5, "OTHER-PART")
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none", sink.binCalls)
	}
	drops := holdDrops(t, db)
	if len(drops) != 1 || drops[0].Reason != protocol.HoldDropForeignHold || drops[0].Qty != 3 {
		t.Errorf("drops = %+v, want one foreign_hold of 3 (the refused attempt's row rolled back)", drops)
	}
}

// ── The hold on every produce node: an unbound seat holds only while a carrier is coming ──
//
// An unbound produce seat holds a tick only while a live order is placing a
// carrier at the node (store.DB.LiveOrderPlacing: one statement over the live
// orders' persisted release facts, a plain order's delivery node, and a
// pre-facts leg read as placing). With none, the tick is logged and not held,
// and a standing pile at the seat is dropped with one hold_dropped
// (no_carrier) row. Unless a pin says otherwise the seat is unbound, holds 5 of
// the claim's part (WIDGET-A), and the tick is 2.

// p2Seat seeds the produce node with its seat unbound and a hold of held parts
// of heldPart (none when held is 0), and wires an engine with a recording sink.
func p2Seat(t *testing.T, held int, heldPart string) (eng *Engine, sink *fakeDeltaSink, processID, nodeID, styleID, claimID int64) {
	t.Helper()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID = seedProduceNode(t, db, protocol.SwapModeTwoRobot)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 0), "empty the seat")
	if held != 0 {
		testutil.MustNoErr(t, processes.AddPendingUOPDelta(db.DB, nodeID, held, heldPart), "seed the hold")
	}
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	sink = &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)
	return eng, sink, processID, nodeID, styleID, claimID
}

// p2Order stores an order the way this Edge holds one and returns its id. A leg
// with steps carries the release facts computed from them (manager_create.go),
// unless preFacts, which is a row older than the column; a leg with no steps is
// a plain order, and its delivery node is where it leaves its bin.
func p2Order(t *testing.T, db *store.DB, status protocol.Status, deliveryNode string, steps []protocol.ComplexOrderStep, preFacts bool) int64 {
	t.Helper()
	typ := protocol.OrderTypeComplex
	if steps == nil {
		typ = protocol.OrderTypeRetrieveEmpty
	}
	id, err := db.CreateOrder(fmt.Sprintf("p2-%s-%d-%d", status, len(steps), time.Now().UnixNano()), typ, nil, steps == nil, 1,
		deliveryNode, "", "", "", false, "WIDGET-A", "", "")
	testutil.MustNoErr(t, err, "create order")
	if steps != nil {
		raw, err := json.Marshal(steps)
		testutil.MustNoErr(t, err, "marshal steps")
		if preFacts {
			testutil.MustNoErr(t, db.UpdateOrderStepsJSON(id, string(raw)), "store steps")
		} else {
			facts, err := release.FactsFromSteps(steps, "PRODUCE-NODE").Encode()
			testutil.MustNoErr(t, err, "encode facts")
			testutil.MustNoErr(t, db.UpdateOrderStepsAndFacts(id, string(raw), facts), "store steps and facts")
		}
	}
	_, err = db.DB.Exec(`UPDATE orders SET status=? WHERE id=?`, string(status), id)
	testutil.MustNoErr(t, err, "set status")
	return id
}

// p2Claim is the fixture's claim, for the step builders.
func p2Claim(t *testing.T, eng *Engine, claimID int64) *processes.NodeClaim {
	t.Helper()
	c, err := eng.db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim")
	return c
}

// p2Supply stores a live two_robot supply leg placing a carrier at the
// fixture's node: the order bringing the next bin.
func p2Supply(t *testing.T, eng *Engine, claimID int64) int64 {
	t.Helper()
	a, _ := BuildTwoRobotSwapSteps(p2Claim(t, eng, claimID))
	return p2Order(t, eng.db, protocol.StatusInTransit, "", a, false)
}

// assertDrops checks the queued hold_dropped rows are exactly want, in order.
func assertDrops(t *testing.T, db *store.DB, want ...protocol.HoldDropped) {
	t.Helper()
	got := holdDrops(t, db)
	if len(got) != len(want) {
		t.Fatalf("drops = %+v, want %d: %+v", got, len(want), want)
	}
	for i, w := range want {
		g := got[i]
		if g.Reason != w.Reason || g.Qty != w.Qty || g.PayloadCode != w.PayloadCode || g.Node != w.Node || g.CalledBy != w.CalledBy {
			t.Errorf("drop %d = %+v, want %+v", i, g, w)
		}
	}
}

// TestUnboundProduceTick_HoldsOnlyWhileAnOrderPlacesACarrier — one row per
// answer of the predicate. Placing: the tick is held on the pile (7). Not
// placing: the pile is dropped with one no_carrier row of 5 and the tick is not
// held (0). No part is tested: a changeover's or a press-index leg carries the
// outgoing part.
func TestUnboundProduceTick_HoldsOnlyWhileAnOrderPlacesACarrier(t *testing.T) {
	t.Parallel()
	single := func(c *processes.NodeClaim) []protocol.ComplexOrderStep { return BuildSingleSwapSteps(c) }
	supply := func(c *processes.NodeClaim) []protocol.ComplexOrderStep { a, _ := BuildTwoRobotSwapSteps(c); return a }
	evac := func(c *processes.NodeClaim) []protocol.ComplexOrderStep { _, b := BuildTwoRobotSwapSteps(c); return b }
	fromSpot := func(c *processes.NodeClaim) []protocol.ComplexOrderStep {
		spot := *c
		spot.KeepStagedNode = "PRODUCE-SPOT"
		a, _ := BuildTwoRobotSwapSteps(&spot)
		return a
	}
	for _, tc := range []struct {
		name     string
		status   protocol.Status
		delivery string // a plain order's delivery node
		steps    func(*processes.NodeClaim) []protocol.ComplexOrderStep
		held     string // release_held: a leg held at a gate
		preFacts bool
		placing  bool
	}{
		{name: "single_robot in flight", status: protocol.StatusInTransit, steps: single, placing: true},
		{name: "two_robot supply staged", status: protocol.StatusStaged, steps: supply, placing: true},
		{name: "two_robot supply held at a gate", status: protocol.StatusStaged, steps: supply, held: "held: the curtain is not safe", placing: true},
		{name: "cancelled", status: protocol.StatusCancelled, steps: supply},
		{name: "keeper refill to the spot", status: protocol.StatusInTransit, delivery: "PRODUCE-SPOT"},
		{name: "spot to line move", status: protocol.StatusInTransit, steps: fromSpot, placing: true},
		{name: "delivered but unbound", status: protocol.StatusDelivered, steps: single, placing: true},
		{name: "no orders"},
		{name: "a pre-facts complex row", status: protocol.StatusInTransit, steps: single, preFacts: true, placing: true},
		{name: "the evac leg alone", status: protocol.StatusStaged, steps: evac},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, sink, processID, nodeID, styleID, claimID := p2Seat(t, 5, "WIDGET-A")
			switch {
			case tc.steps != nil:
				id := p2Order(t, eng.db, tc.status, "", tc.steps(p2Claim(t, eng, claimID)), tc.preFacts)
				if tc.held != "" {
					_, err := eng.db.DB.Exec(`UPDATE orders SET release_held=? WHERE id=?`, tc.held, id)
					testutil.MustNoErr(t, err, "hold the leg")
				}
			case tc.delivery != "":
				p2Order(t, eng.db, tc.status, tc.delivery, nil, false)
			}

			charSPTick(eng, processID, styleID, 2)

			if len(sink.binCalls) != 0 {
				t.Errorf("deltas = %+v, want none: nothing is bound", sink.binCalls)
			}
			if tc.placing {
				assertSeat(t, eng, nodeID, 0, 7, "WIDGET-A")
				assertDrops(t, eng.db)
				return
			}
			assertSeat(t, eng, nodeID, 0, 0, "")
			assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropNoCarrier, Qty: 5,
				PayloadCode: "WIDGET-A", Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
		})
	}
}

// TestUnboundProduceTick_ForeignHoldWithNoCarrierIsDroppedAndTheTickLogged —
// an unbound seat holding 5 of ANOTHER part, no live order. Nothing is coming
// for that hold or for this tick, so the general rule applies to both: the
// foreign hold is dropped with a no_carrier row and this tick is logged, not
// held (dropPileAndLogTick, the same branch as a hold of the tick's own part).
// The following tick finds an empty seat with none coming: logged, no row.
func TestUnboundProduceTick_ForeignHoldWithNoCarrierIsDroppedAndTheTickLogged(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, _ := p2Seat(t, 5, "OTHER-PART")

	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, 0, 0, "")
	foreign := protocol.HoldDropped{Reason: protocol.HoldDropNoCarrier, Qty: 5, PayloadCode: "OTHER-PART",
		Node: "PRODUCE-NODE", CalledBy: "edge:tick"}
	assertDrops(t, eng.db, foreign)
	if got := eng.carrierNoteLogged[nodeID]; got != holdNoteNoCarrier {
		t.Errorf("episode note = %q, want %q: the tick is logged", got, holdNoteNoCarrier)
	}

	charSPTick(eng, processID, styleID, 3)
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db, foreign)
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none", sink.binCalls)
	}
}

// TestUnboundProduceTick_ForeignHoldWithACarrierComingStays — the predicate's
// other answer: an unbound seat holding 5 of ANOTHER part while a live order is
// placing a carrier at the node. The foreign hold stays for its part's next
// carrier (D12), and this tick is neither held nor charged, noted once for the
// episode (noteUnheldTick). No row.
func TestUnboundProduceTick_ForeignHoldWithACarrierComingStays(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, claimID := p2Seat(t, 5, "OTHER-PART")
	p2Supply(t, eng, claimID)

	charSPTick(eng, processID, styleID, 2)
	charSPTick(eng, processID, styleID, 3)

	assertSeat(t, eng, nodeID, 0, 5, "OTHER-PART")
	assertDrops(t, eng.db)
	if got := eng.unheldTickLogged[nodeID]; got != "OTHER-PART|WIDGET-A" {
		t.Errorf("unheld episode = %q, want the pair of parts noted", got)
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("deltas = %+v, want none", sink.binCalls)
	}
}

// TestUnboundProduceTick_AnUnreadableOrderKeepsThePile — a live leg whose
// release facts do not parse might be placing a carrier here, so the predicate
// fails closed: the tick is held as before and nothing is dropped.
func TestUnboundProduceTick_AnUnreadableOrderKeepsThePile(t *testing.T) {
	t.Parallel()
	eng, _, processID, nodeID, styleID, claimID := p2Seat(t, 5, "WIDGET-A")
	id := p2Order(t, eng.db, protocol.StatusInTransit, "", BuildSingleSwapSteps(p2Claim(t, eng, claimID)), false)
	_, err := eng.db.DB.Exec(`UPDATE orders SET release_facts='{"places":' WHERE id=?`, id)
	testutil.MustNoErr(t, err, "corrupt the facts")

	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, 0, 7, "WIDGET-A")
	assertDrops(t, eng.db)
}

// TestUnboundProduceTick_StrokesWithNoBinAreOneRowAtTheNextBind — a
// fork-truck-fed seat between bins: three ticks (1, 2, 4) on an unbound seat
// with no live order are logged and not held, and nothing is sent while the
// episode is open. The next bin binds; its first tick counts +1 and, in the
// same transaction, the episode goes to Core as one no_bin row of 7. A later
// tick sends nothing more.
func TestUnboundProduceTick_StrokesWithNoBinAreOneRowAtTheNextBind(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID, _ := p2Seat(t, 0, "")
	for _, d := range []int64{1, 2, 4} {
		charSPTick(eng, processID, styleID, d)
	}
	assertSeat(t, eng, nodeID, 0, 0, "")
	assertDrops(t, eng.db)

	bin := int64(7601)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &bin), "the next bin binds")
	assertDrops(t, eng.db)
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1", sink.binCalls)
	}
	noBin := protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 7, PayloadCode: "WIDGET-A",
		Node: "PRODUCE-NODE", CalledBy: "edge:tick"}
	assertDrops(t, eng.db, noBin)

	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 2, 0, "")
	assertDrops(t, eng.db, noBin)
}

// TestUnboundProduceTick_StrokesWithNoBinEndWhenACarrierIsComing — the other
// end of the episode: a supply leg starts placing a carrier, and the tick that
// finds it is held for that carrier while the episode's 3 logged strokes go as
// one no_bin row in the hold's transaction.
func TestUnboundProduceTick_StrokesWithNoBinEndWhenACarrierIsComing(t *testing.T) {
	t.Parallel()
	eng, _, processID, nodeID, styleID, claimID := p2Seat(t, 0, "")
	charSPTick(eng, processID, styleID, 3)
	assertDrops(t, eng.db)
	p2Supply(t, eng, claimID)
	charSPTick(eng, processID, styleID, 2)
	assertSeat(t, eng, nodeID, 0, 2, "WIDGET-A")
	assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropNoBin, Qty: 3, PayloadCode: "WIDGET-A",
		Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
}

// TestUnboundProduceTick_AnUnreadableOrderReachesTheDiagnosticsPage — the
// kept pile's journal line also goes to the Edge diagnostics page (debugFn),
// the same text under the same once-per-episode gate: two ticks, one line.
func TestUnboundProduceTick_AnUnreadableOrderReachesTheDiagnosticsPage(t *testing.T) {
	t.Parallel()
	eng, _, processID, _, styleID, claimID := p2Seat(t, 5, "WIDGET-A")
	var mu sync.Mutex
	var debug []string
	eng.debugFn = func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		debug = append(debug, fmt.Sprintf(f, a...))
	}
	id := p2Order(t, eng.db, protocol.StatusInTransit, "", BuildSingleSwapSteps(p2Claim(t, eng, claimID)), false)
	_, err := eng.db.DB.Exec(`UPDATE orders SET release_facts='{"places":' WHERE id=?`, id)
	testutil.MustNoErr(t, err, "corrupt the facts")

	charSPTick(eng, processID, styleID, 2)
	charSPTick(eng, processID, styleID, 2)

	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, line := range debug {
		if strings.Contains(line, "node PRODUCE-NODE: a live order's release facts cannot be read") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("diagnostics lines for the unreadable order = %d, want 1; debug: %q", n, debug)
	}
}

// TestTakeTicksAfterLift_AProduceHoldCarriesTheClaimsPart — the ticks counted
// after the robot lifted a produce bin are held for the next bin with the
// claim's part (producedPayload). The bin's resident payload is "" for the
// whole fill (a known-empty delivery), and a "" hold is a wildcard that would
// replay onto any part.
func TestTakeTicksAfterLift_AProduceHoldCarriesTheClaimsPart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedProduceNode(t, db, protocol.SwapModeTwoRobot)
	bin := int64(7401)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &bin, 3, 40), "bind at generation 3")
	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&afterLiftSink{sum: 4})
	eng.recordLinesideCarrier(nodeID, "PRODUCE-NODE", domain.KnownCarrier(""), domain.CarrierFromDelivery)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	rt := runtimeOf(t, eng, nodeID)

	held, part := eng.takeTicksAfterLift(node, rt, protocol.BinPickedUp{BinID: bin, Epoch: 4, PickedUpAt: time.Now()})
	if held != 4 || part != "WIDGET-A" {
		t.Errorf("hold = %d of %q, want 4 of WIDGET-A (the claim's part, not the empty carrier's \"\")", held, part)
	}
}

// afterLiftSink is a recording sink whose after-lift windows sum to sum.
type afterLiftSink struct {
	fakeDeltaSink
	sum int
}

func (s *afterLiftSink) TakeAfterLift(int64, int64, time.Time) (int, error) { return s.sum, nil }
