package engine

import (
	"testing"

	"shingo/protocol/testutil"
)

// TestStagedUnbound_TicksParkInPendingNoBinEverBound is the F1a bug reproduction
// (P2-C1). It pins the CURRENT steady-state that produced the SNF3 CARRIER-0024
// incident (2026-07-24): a bin physically delivered to a consuming lineside node
// whose order never reached `delivered` — stuck at `staged`, or the Edge
// delivered-gate declined to bind — leaves the node's runtime with NO active
// bin. Consume ticks then keep arriving and:
//
//   - active_bin_id never points at the new bin (nothing binds it),
//   - the ticks pile up in pending_uop_delta (held, not charged),
//   - NOT ONE BinUOPDelta ever carries the new bin's ID,
//
// so Core's bins.uop_remaining for that carrier can never move from its
// delivered snapshot while the physical line drains to empty. That divergence
// is the silent primary disease — the tile reads the truth (46) while Core's
// bin (and therefore the on-hand SUM the replenishment monitor trusts) holds
// the stale delivered value.
//
// This test stays GREEN as the characterization anchor. The later Phase-2
// commits do NOT auto-heal this state (that would need F1b / a Core-side
// delivered fix, both out of scope tonight); they make it survivable:
//   - P2-C3 makes the delivered-gate skips scream instead of no-op'ing,
//   - P2-C5 lets a front-door count correction BIND this stranded bin,
//   - P2-C7 raises the parked-ticks / staged-age alarm that names the bin.
//
// If a future change silently binds a staged bin (papering over the symptom
// rather than fixing the producer), this assertion trips first — by design.
func TestStagedUnbound_TicksParkInPendingNoBinEverBound(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "STAGED-UNBOUND",
		PayloadCode: "PART-SNF3",
		UOPCapacity: 150,
		InitialUOP:  0,
	})

	// The new carrier (CARRIER-0024 analog, bin id 24) is delivered physically
	// but its order is stuck at `staged` — the OrderDelivered event never fires,
	// so handleNodeOrderDelivered never binds. Model that durable runtime state
	// directly: an empty slot (no active bin). seedConsumeNode defaults a bin at
	// the slot, so clear it.
	const newBinID int64 = 24
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil),
		"clear active bin (delivered-but-staged: nothing bound)")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{}
	eng.SetInventoryDeltaSink(sink)

	tick := func(d int) {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: int64(d),
		}})
	}

	// The line runs: consume ticks keep arriving at the node (46 units drawn,
	// mirroring the incident's tile falling to 46).
	tick(20)
	tick(20)
	tick(6)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")

	// 1) active_bin_id never points at the new bin — nothing bound it.
	if rt.ActiveBinID != nil {
		t.Fatalf("ActiveBinID = %v, want nil — a staged-but-unbound delivery must NOT bind (that IS the bug); got a binding",
			rt.ActiveBinID)
	}

	// 2) the ticks pile up in pending_uop_delta, held rather than charged.
	if rt.PendingUOPDelta != 46 {
		t.Errorf("PendingUOPDelta = %d, want 46 (every consume tick parked while the slot is unbound)",
			rt.PendingUOPDelta)
	}

	// 3) the cache never moved — held ticks must not touch remaining while unbound.
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOPCached = %d, want 0 (held ticks do not decrement an unbound slot)",
			rt.RemainingUOPCached)
	}

	// 4) NOT ONE BinUOPDelta ever carried the new bin's ID (nor any other) — so
	//    Core's bins.uop_remaining for CARRIER-0024 can never move. binAtNode
	//    returns bin id 0 for an unbound slot, which the mutator skips.
	for _, bc := range sink.binCalls {
		if bc.BinID == newBinID {
			t.Fatalf("a BinUOPDelta carried the new bin id %d (%+v) — the repro expects ZERO; the stream is detached",
				newBinID, bc)
		}
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("binCalls = %d, want 0 (unbound slot → no bin delta emitted): %+v",
			len(sink.binCalls), sink.binCalls)
	}
}
