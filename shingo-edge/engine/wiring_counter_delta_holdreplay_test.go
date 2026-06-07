package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
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
