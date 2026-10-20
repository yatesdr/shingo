package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
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
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "clear active bin (open the gap)")

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

// TestCharSP_UnboundProduceTickIsHeldUncapped — a produce seat with no bin
// bound and a standing hold of 3,000 of the claim's part (pack 100) takes a
// tick of 2. Today: held, uncapped. The hold becomes 3,002 with the part
// recorded, nothing is charged, no produce_tick is emitted, and nothing asks
// whether any order is placing a carrier here (none exists).
//
// P2 changes this: an unbound produce tick that would hold with no live order
// placing a carrier at the node drops the standing pile with a hold_dropped
// (no_carrier) row instead of growing it.
func TestCharSP_UnboundProduceTickIsHeldUncapped(t *testing.T) {
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
	if rt.PendingUOPDelta != held+2 {
		t.Errorf("hold = %d, want %d: an unbound produce tick is held on top of the pile, uncapped", rt.PendingUOPDelta, held+2)
	}
	if rt.PendingUOPPayload != "WIDGET-A" {
		t.Errorf("hold part = %q, want WIDGET-A", rt.PendingUOPPayload)
	}
	if rt.RemainingUOPCached != 0 || rt.ActiveBinID != nil {
		t.Errorf("seat = bin %v count %d, want unbound at 0: nothing is charged", rt.ActiveBinID, rt.RemainingUOPCached)
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("bin deltas = %+v, want none: a held tick emits no produce_tick", sink.binCalls)
	}
}

// TestCharSP_FirstBoundTickReplaysTheWholeHold — the first tick after a bin of
// the held part binds charges input + held in one write (ChargeSeatUOP with
// clearPending), clears the hold and its part, and emits ONE produce_tick of
// input + held against the bin. The second case holds more than the pack: the
// count passes the pack (3,001 on a pack of 100), because nothing on the
// produce path reads the pack today.
//
// P1's strict arm caps this (charge MIN(avail, pack - count), the rest stays in
// the hold, the emit is the charged amount); P2's backstop drops a hold above
// the pack with a hold_dropped (backstop) row and charges the input alone.
func TestCharSP_FirstBoundTickReplaysTheWholeHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		held int
	}{
		{"hold under the pack", 5},
		{"hold over the pack", 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, sink, processID, nodeID, styleID, _ := charSPProduceSeat(t, tc.held)
			const binID int64 = 7201
			b := binID
			testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &b), "bind a bin of the part")

			charSPTick(eng, processID, styleID, 1)

			rt := runtimeOf(t, eng, nodeID)
			if want := tc.held + 1; rt.RemainingUOPCached != want {
				t.Errorf("bin count = %d, want %d (bound 0 + held %d + tick 1, one write)", rt.RemainingUOPCached, want, tc.held)
			}
			if rt.PendingUOPDelta != 0 || rt.PendingUOPPayload != "" {
				t.Errorf("hold = %d of %q, want cleared", rt.PendingUOPDelta, rt.PendingUOPPayload)
			}
			if len(sink.binCalls) != 1 {
				t.Fatalf("bin deltas = %+v, want exactly one", sink.binCalls)
			}
			bc := sink.binCalls[0]
			if bc.BinID != binID || bc.Delta != tc.held+1 || bc.Reason != protocol.ReasonProduceTick || bc.PayloadCode != "WIDGET-A" {
				t.Errorf("delta = %+v, want {bin %d, +%d, produce_tick, WIDGET-A}", bc, binID, tc.held+1)
			}
		})
	}
}
