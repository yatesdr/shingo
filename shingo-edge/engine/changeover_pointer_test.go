// changeover_pointer_test.go — the pointer verb: a flip points every node the
// incoming style claims at that claim, by name and never to NULL, without
// touching the count; a cancel points the swapped nodes back at the staying
// style; the flip syncs the reporting point itself.
package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// pointerRig is seedChangeoverScenario plus a second node both styles claim
// and a node only the from-style claims, every runtime on its from-claim.
func pointerRig(t *testing.T) (eng *Engine, db *store.DB, pid, from, to int64, seats map[string]pointerSeat) {
	t.Helper()
	db = testEngineDB(t)
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	var nodeID, fromClaim, toClaim int64
	pid, nodeID, from, to, fromClaim, toClaim = seedChangeoverScenario(t, db)
	seats = map[string]pointerSeat{"CO-NODE": {nodeID, fromClaim, toClaim}}
	for _, n := range []struct {
		name string
		both bool
	}{{"CO-NODE-2", true}, {"CO-FROM-ONLY", false}} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: n.name, Code: n.name, Name: n.name, Enabled: true})
		testutil.MustNoErr(t, err, "node "+n.name)
		f, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: from, CoreNodeName: n.name, Role: "consume",
			SwapMode: "simple", PayloadCode: "PART-OLD", UOPCapacity: 100, InboundSource: "SRC"})
		testutil.MustNoErr(t, err, "from claim "+n.name)
		var tc int64
		if n.both {
			tc, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: to, CoreNodeName: n.name, Role: "consume",
				SwapMode: "simple", PayloadCode: "PART-NEW", UOPCapacity: 100, InboundSource: "SRC"})
			testutil.MustNoErr(t, err, "to claim "+n.name)
		}
		_, err = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "runtime "+n.name)
		testutil.MustNoErr(t, db.SetProcessNodeRuntime(id, &f, 37), "seat "+n.name)
		seats[n.name] = pointerSeat{id, f, tc}
	}
	return eng, db, pid, from, to, seats
}

type pointerSeat struct{ node, fromClaim, toClaim int64 }

func pointerOf(t *testing.T, eng *Engine, nodeID int64) int64 {
	t.Helper()
	rt := runtimeOf(t, eng, nodeID)
	if rt.ActiveClaimID == nil {
		t.Fatalf("node %d's pointer is NULL", nodeID)
	}
	return *rt.ActiveClaimID
}

// TestFlip_PointsEveryClaimedNodeAtTheIncomingStyle: after a flip every node
// the incoming style claims points at that claim — the record of what the
// node runs, which the evacuation route and the release read.
func TestFlip_PointsEveryClaimedNodeAtTheIncomingStyle(t *testing.T) {
	t.Parallel()
	eng, _, pid, _, to, seats := pointerRig(t)
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "flip")
	for _, name := range []string{"CO-NODE", "CO-NODE-2"} {
		if got := pointerOf(t, eng, seats[name].node); got != seats[name].toClaim {
			t.Errorf("%s points at claim %d, want the incoming style's %d", name, got, seats[name].toClaim)
		}
	}
}

// TestFlip_NeverNullsAPointer: a node the incoming style does not claim keeps
// its pointer — a NULL one would drop the operator's release with no count.
func TestFlip_NeverNullsAPointer(t *testing.T) {
	t.Parallel()
	eng, _, pid, _, to, seats := pointerRig(t)
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "flip")
	if got := pointerOf(t, eng, seats["CO-FROM-ONLY"].node); got != seats["CO-FROM-ONLY"].fromClaim {
		t.Errorf("an unclaimed node's pointer moved to %d", got)
	}
}

// TestFlip_LeavesTheCount: the pointer write is the claim only; every count
// stands.
func TestFlip_LeavesTheCount(t *testing.T) {
	t.Parallel()
	eng, _, pid, _, to, seats := pointerRig(t)
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "flip")
	for name, s := range seats {
		if name == "CO-NODE" {
			continue // seeded at 50 by seedChangeoverScenario
		}
		if got := runtimeOf(t, eng, s.node).RemainingUOPCached; got != 37 {
			t.Errorf("%s count = %d after the flip, want 37", name, got)
		}
	}
}

// TestCancel_PointsTheSwappedNodesBackAtTheStayingStyle: a node whose incoming
// cart landed points at the to-claim; the cancel keeps the style, so the node
// points back at the staying claim.
func TestCancel_PointsTheSwappedNodesBackAtTheStayingStyle(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, to, seats := pointerRig(t)
	startChangeover(t, eng, db, pid, to)
	s := seats["CO-NODE"]
	testutil.MustNoErr(t, db.SetProcessNodeActiveClaim(s.node, &s.toClaim), "the incoming cart landed")
	testutil.MustNoErr(t, eng.CancelProcessChangeover(pid), "cancel")
	if got := pointerOf(t, eng, s.node); got != s.fromClaim {
		t.Errorf("after the cancel the node points at %d, want the staying claim %d", got, s.fromClaim)
	}
}

// TestFlip_SyncsThePointInsideTheVerb: the reporting point is on the incoming
// style when the flip returns, so the next tick the poll emits (stamped with
// the point's style) is charged, not dropped as style_mismatch.
func TestFlip_SyncsThePointInsideTheVerb(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.plcMgr = seedRigManager()
	pid, nodeID, _, to, _, toClaim := seedChangeoverScenario(t, db)
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "CNT", true, "", false), "counter")
	testutil.MustNoErr(t, eng.syncPrimaryPoint(pid, "", ""), "the point, on the from-style")

	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "flip")
	rp, err := db.GetReportingPointByTag("plc", "CNT")
	testutil.MustNoErr(t, err, "point")
	if rp.StyleID != to {
		t.Fatalf("point on style %d after the flip, want %d", rp.StyleID, to)
	}
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &toClaim, binPtr(9201), 40), "a cart on the to-claim")
	tickConsume(eng, pid, rp.StyleID, 1)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 39 {
		t.Errorf("cart after one tick = %d, want 39: the tick was not charged", got)
	}
}
