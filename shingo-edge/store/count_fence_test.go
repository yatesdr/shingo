package store

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestReleaseCountWithCursor_WritesTheCountAndReadsTheCursorInOneStatement:
// the release's count write returns the carrier's stream position (Lane H,
// DECISIONS D11 (i)): zero for a scope that has flushed nothing, the last seq
// and the running net once it has. A count of 0 marks the seat released-empty.
func TestReleaseCountWithCursor_WritesTheCountAndReadsTheCursorInOneStatement(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, err := db.CreateProcess("RCC-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "RCC-1", Name: "RCC-1", Enabled: true})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	seq, net, found, err := db.ReleaseCountWithCursor(nodeID, 7, protocol.InvDeltaScopeBin, "4401", 3)
	testutil.MustNoErr(t, err, "release count, empty scope")
	if !found || seq != 0 || net != 0 {
		t.Fatalf("empty scope: found=%t seq=%d net=%d, want true 0 0", found, seq, net)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.RemainingUOPCached != 7 || rt.ReleasedEmpty {
		t.Fatalf("after a release of 7: count %d released_empty %t", rt.RemainingUOPCached, rt.ReleasedEmpty)
	}

	for _, add := range []int64{-4, -6} {
		_, _, err := db.AllocateInventoryDeltaSeq(protocol.InvDeltaScopeBin, "4401", 3, add)
		testutil.MustNoErr(t, err, "allocate a window")
	}
	_, _, err = db.AllocateInventoryDeltaSeq(protocol.InvDeltaScopeBin, "4401", 4, -1) // another generation
	testutil.MustNoErr(t, err, "allocate in another generation")
	seq, net, found, err = db.ReleaseCountWithCursor(nodeID, 0, protocol.InvDeltaScopeBin, "4401", 3)
	testutil.MustNoErr(t, err, "release count, flushed scope")
	if !found || seq != 2 || net != -10 {
		t.Fatalf("flushed scope: found=%t seq=%d net=%d, want true 2 -10", found, seq, net)
	}
	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.RemainingUOPCached != 0 || !rt.ReleasedEmpty {
		t.Fatalf("after a release of 0: count %d released_empty %t, want 0 and true", rt.RemainingUOPCached, rt.ReleasedEmpty)
	}

	if _, _, found, err := db.ReleaseCountWithCursor(nodeID+999, 5, protocol.InvDeltaScopeBin, "4401", 3); err != nil || found {
		t.Fatalf("a node with no runtime row: found=%t err=%v, want false nil", found, err)
	}
}
