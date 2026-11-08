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

// TestPinForcedCutover_AHeldBackLegsStrokesAreLogged — the plant's pattern: a
// changeover with its gate red (the position's leg held back, so its outgoing
// cart, landed and known PART-OLD, stays bound), the PLC changes to the
// incoming part and the first counted part forces the cutover. Every incoming
// stroke at the position, before the force and after it, is logged at the
// position (decision 55) and never charged to the outgoing cart; when the
// incoming cart lands it takes its own strokes from its full count, stamped
// PART-NEW. Before decision 38, each was charged to the outgoing cart, stamped
// PART-OLD, and Core accepted it: the wrong cart.
func TestPinForcedCutover_AHeldBackLegsStrokesAreLogged(t *testing.T) {
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
	if s := readSeat55(t, eng, pid, nodeID); s.hold != 0 || s.logged != 5 {
		t.Fatalf("seat = hold %d, logged %d; want none held, 5 logged", s.hold, s.logged)
	}

	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "P3-NODE"), "the held leg lifts the outgoing cart")
	eng.HandleBinLanded(landing("P3-NODE", 9952, 1, 60, "PART-NEW", 41))
	mark = len(sink.binCalls)
	countedStroke(eng, pid, to)
	if s := readSeat55(t, eng, pid, nodeID); s.cart != 59 || s.hold != 0 || s.logged != 5 {
		t.Errorf("the incoming cart after one stroke = %d, hold %d, logged %d; want 59 (its own stroke), none, the 5 still logged",
			s.cart, s.hold, s.logged)
	}
	if d := sink.binCalls[mark:]; len(d) != 1 || d[0].BinID != 9952 || d[0].Delta != -1 || d[0].PayloadCode != "PART-NEW" {
		t.Errorf("deltas = %+v, want -1 on the incoming cart 9952 stamped PART-NEW", d)
	}
}

func mustNodeIDByName(t *testing.T, eng *Engine, core string) int64 {
	t.Helper()
	n, err := eng.db.GetProcessNodeByCoreNodeName(core)
	testutil.MustNoErr(t, err, "node "+core)
	return n.ID
}
