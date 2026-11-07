// catid_force_held_leg_test.go — a leg held back past a forced cutover: the
// outgoing cart stays bound, and the incoming part's strokes there wait for
// the incoming cart.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/plc"
)

// TestPinForcedCutover_AHeldBackLegHoldsItsStrokesUntilItsCartLands — the
// plant's pattern: a changeover with its gate red (the position's leg held
// back, so its outgoing cart, landed and known PART-OLD, stays bound), the
// PLC changes to the incoming part and the first counted part forces the
// cutover. Every incoming stroke at the position, before the force and after
// it, is held at the seat under PART-NEW and never charged to the outgoing
// cart; when the incoming cart lands, the next stroke charges it with all of
// them, stamped PART-NEW. Before, each was charged to the outgoing cart,
// stamped PART-OLD, and Core accepted it: the wrong cart.
func TestPinForcedCutover_AHeldBackLegHoldsItsStrokesUntilItsCartLands(t *testing.T) {
	t.Parallel()
	eng, db, pid, from, coID, _, cm := forceRig(t, "auto")
	nodeID := mustNodeIDByName(t, eng, "P3-NODE")
	fromClaim := runtimeOf(t, eng, nodeID).ActiveClaimID
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, fromClaim, binPtr(9951), 40), "the outgoing cart")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-OLD", true, "landing"), "landed PART-OLD")
	sink := eng.inventoryDelta.(*fakeDeltaSink)
	mark := len(sink.binCalls)

	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	settle(cm, pid, st, "1002", time.Now())
	if id := countedStroke(eng, pid, from); id.Kind != plc.TickTarget {
		t.Fatalf("the first counted incoming part = %+v, want the target's", id)
	}
	drainPokes(eng, cm)
	tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(61*time.Second))
	if co := changeoverRow(t, db, coID); co.State != domain.ChangeoverCompleted || co.TriggeredBy != triggeredAutoCATID+triggeredForced {
		t.Fatalf("changeover = %s by %q, want completed by auto-catid-forced", co.State, co.TriggeredBy)
	}
	to := *mustProcess(t, db, pid).ActiveStyleID
	for i := 0; i < 4; i++ {
		countedStroke(eng, pid, to)
	}
	rt := runtimeOf(t, eng, nodeID)
	var charged []fakeBinCall
	for _, c := range sink.binCalls[mark:] {
		if c.BinID == 9951 {
			charged = append(charged, c)
		}
	}
	if rt.RemainingUOPCached != 40 || len(charged) != 0 {
		t.Fatalf("the outgoing cart after 5 incoming strokes = %d, deltas on it %+v; want 40 and none: the strokes are "+
			"PART-NEW's, not that cart's", rt.RemainingUOPCached, charged)
	}
	if rt.PendingUOPDelta != 5 || rt.PendingUOPPayload != "PART-NEW" {
		t.Fatalf("seat = hold %d of %q, want 5 of PART-NEW", rt.PendingUOPDelta, rt.PendingUOPPayload)
	}

	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "P3-NODE"), "the held leg lifts the outgoing cart")
	eng.HandleBinLanded(landing("P3-NODE", 9952, 1, 60, "PART-NEW", 41))
	mark = len(sink.binCalls)
	countedStroke(eng, pid, to)
	if got := runtimeOf(t, eng, nodeID); got.RemainingUOPCached != 54 || got.PendingUOPDelta != 0 {
		t.Errorf("the incoming cart after one stroke = %d, hold %d; want 54 (the 5 held and the 1), hold 0",
			got.RemainingUOPCached, got.PendingUOPDelta)
	}
	if d := sink.binCalls[mark:]; len(d) != 1 || d[0].BinID != 9952 || d[0].Delta != -6 || d[0].PayloadCode != "PART-NEW" {
		t.Errorf("deltas = %+v, want -6 on the incoming cart 9952 stamped PART-NEW", d)
	}
}

func mustNodeIDByName(t *testing.T, eng *Engine, core string) int64 {
	t.Helper()
	n, err := eng.db.GetProcessNodeByCoreNodeName(core)
	testutil.MustNoErr(t, err, "node "+core)
	return n.ID
}
