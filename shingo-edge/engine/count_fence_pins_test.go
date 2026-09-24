package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/uop"
)

// Characterisation pins for the Edge half of the record-count race
// (SYNTH-round2 S7, citrine-kestrel §8 S4). Core writes a counted number N
// absolutely and broadcasts it; the Edge writes N over whatever it holds.

// fenceFixture binds bin 4401 at epoch 3 on a fresh node and wires the real
// accumulator (never started, so only an explicit Flush sends). Returns the
// engine, the node id, the accumulator and the bin id.
func fenceFixture(t *testing.T, coreNode string) (*Engine, int64, *uop.Mutator, int64) {
	t.Helper()
	eng := newCoverageEngine(t)
	nodeID, binID := boundNodeFixture(t, eng, coreNode, 4401, 3)
	acc := uop.New(eng.db, eng.cfg.StationID(), eng.db, eng.db, eng.db)
	eng.SetInventoryDeltaSink(acc)
	return eng, nodeID, acc, binID
}

func runtimeCount(t *testing.T, eng *Engine, nodeID int64) int {
	t.Helper()
	rt, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	return rt.RemainingUOPCached
}

// TestPin_P0f_EdgeTakesCoresNumberOverItsInFlightTicks is P0f end to end,
// the Edge half. The Core half is service.TestRecordCount_SameEpochDeltaAfterTheCountAppliesOnTop.
//
// The station has flushed two windows for the bin: -10 (seq 1, which Core has
// applied) and -5 (seq 2, still in flight when an operator counts 50 at
// Core). Core writes 50, then applies the -5 on top: Core reads 45. Core's
// broadcast says 50 and the Edge writes 50. The two sides now disagree by the
// in-flight 5, and neither knows it.
//
// VERIFY-RED: the record-count fence (S7 second half) stamps Core's
// applied_net and last_seq on the adjustment, and the Edge rebases:
// remaining = 50 + (flushed_net -15 - AsOfNet -10) + unflushed 0 = 45. Both
// sides then read 45.
func TestPin_P0f_EdgeTakesCoresNumberOverItsInFlightTicks(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "FENCE-P0F")

	acc.RecordBin(binID, "PART-A", -10, protocol.ReasonConsumeTick, 3)
	acc.Flush() // seq 1, net -10: Core has applied it
	acc.RecordBin(binID, "PART-A", -5, protocol.ReasonConsumeTick, 3)
	acc.Flush() // seq 2, net -15: in flight when the count lands

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: "FENCE-P0F", NewRemaining: 50, Epoch: 3,
		Actor: "operator-under-test",
	})

	if got := runtimeCount(t, eng, nodeID); got != 50 {
		t.Errorf("Edge count = %d, want 50 (today the Edge takes Core's number as is; Core "+
			"will read 45 once the in-flight -5 lands)", got)
	}
}

// TestPin_OlderAdjustmentArrivingLateOverwritesTheNewer pins that the Edge
// has no way to order two count adjustments for the same carrier. A count of
// 40 is taken after a count of 60; if the 60 is delivered second (an outbox
// retry after the 40 went through), the Edge ends at 60.
//
// VERIFY-RED: the fence records the adjustment's AsOfSeq on the runtime row and
// ignores one whose AsOfSeq is below the recorded one for the same (bin,
// epoch); the Edge then keeps 40.
func TestPin_OlderAdjustmentArrivingLateOverwritesTheNewer(t *testing.T) {
	t.Parallel()
	eng, nodeID, _, binID := fenceFixture(t, "FENCE-ORDER")

	newer := protocol.UOPAdjustment{BinID: binID, CoreNodeName: "FENCE-ORDER", NewRemaining: 40, Epoch: 3, Actor: "operator-under-test"}
	older := protocol.UOPAdjustment{BinID: binID, CoreNodeName: "FENCE-ORDER", NewRemaining: 60, Epoch: 3, Actor: "operator-under-test"}
	eng.HandleUOPAdjustment(newer)
	eng.HandleUOPAdjustment(older)

	if got := runtimeCount(t, eng, nodeID); got != 60 {
		t.Errorf("Edge count = %d, want 60 (today the late, older count overwrites the newer)", got)
	}
}
