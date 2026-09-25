package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/uop"
)

// TestPin_P0j_AbandonedAccumulatorMovedTheRuntimeAndWroteNoRow pins P0j
// (V11). A PLC tick writes the node's runtime count at once, but the count
// message for it sits in the accumulator's memory until the next flush. If the
// process dies before that (a hard crash: no Stop, no Flush), the runtime has
// moved and there is no outbox row, so Core never hears of those parts.
//
// Characterisation only. This build does not invert it (SYNTH-round2 §4, the
// boot reconcile is ruled out); the round-1 checksum sees the gap as a
// standing divergence.
func TestPin_P0j_AbandonedAccumulatorMovedTheRuntimeAndWroteNoRow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABPair(t, db)

	const binA, binB int64 = 8001, 8002
	orderA := stageABOrder(t, db, nodeAID, "uuid-abandon-A", "PART-AB", binA)
	orderB := stageABOrder(t, db, nodeBID, "uuid-abandon-B", "PART-AB", binB)
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeAID, &orderA, nil), "set A active order")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeBID, &orderB, nil), "set B active order")
	bidA, bidB := binA, binB
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeAID, &bidA), "bind A")
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeBID, &bidB), "bind B")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	// The production accumulator, never started and never stopped: its
	// memory is what a crash throws away.
	acc := uop.New(db, "stn-test", db, db)
	eng.SetInventoryDeltaSink(acc)

	before, err := db.GetProcessNodeRuntime(nodeAID)
	testutil.MustNoErr(t, err, "read runtime before")

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 7,
	}})

	after, err := db.GetProcessNodeRuntime(nodeAID)
	testutil.MustNoErr(t, err, "read runtime after")
	if after.RemainingUOPCached == before.RemainingUOPCached {
		t.Fatalf("runtime count did not move (%d) — the tick never landed, so this pin shows nothing",
			after.RemainingUOPCached)
	}

	msgs, err := db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType == protocol.SubjectBinUOPDelta {
			t.Fatalf("an outbox row exists for the tick before any flush; P0j says the count lives only in memory")
		}
	}

	// Control: the count was in the accumulator, so a flush (which a crash
	// never gets) writes it.
	acc.Flush()
	msgs, err = db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox after flush")
	found := false
	for _, m := range msgs {
		found = found || m.MsgType == protocol.SubjectBinUOPDelta
	}
	if !found {
		t.Fatal("no bin_uop_delta row even after a flush — the tick never reached the accumulator, so this pin shows nothing")
	}
}
