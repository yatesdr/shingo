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

	seven := 7
	written, seq, net, found, err := db.ReleaseCountWithCursor(nodeID, &seven, protocol.InvDeltaScopeBin, "4401", 3)
	testutil.MustNoErr(t, err, "release count, empty scope")
	if !found || seq != 0 || net != 0 || written != 7 {
		t.Fatalf("empty scope: found=%t seq=%d net=%d written=%d, want true 0 0 7", found, seq, net, written)
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
	zero := 0
	written, seq, net, found, err = db.ReleaseCountWithCursor(nodeID, &zero, protocol.InvDeltaScopeBin, "4401", 3)
	testutil.MustNoErr(t, err, "release count, flushed scope")
	if !found || seq != 2 || net != -10 || written != 0 {
		t.Fatalf("flushed scope: found=%t seq=%d net=%d written=%d, want true 2 -10 0", found, seq, net, written)
	}
	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.RemainingUOPCached != 0 || !rt.ReleasedEmpty {
		t.Fatalf("after a release of 0: count %d released_empty %t, want 0 and true", rt.RemainingUOPCached, rt.ReleasedEmpty)
	}

	five := 5
	if _, _, _, found, err := db.ReleaseCountWithCursor(nodeID+999, &five, protocol.InvDeltaScopeBin, "4401", 3); err != nil || found {
		t.Fatalf("a node with no runtime row: found=%t err=%v, want false nil", found, err)
	}
}

// TestReleaseCountWithCursor_NoCountDeclaresTheSeatsOwn: with no declared
// count the statement writes the seat's count as it stands (decision 44a), so
// the count and the fence are read in the one instant; floored at 0, with
// released_empty following the value written.
func TestReleaseCountWithCursor_NoCountDeclaresTheSeatsOwn(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, err := db.CreateProcess("RCS-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "RCS-1", Name: "RCS-1", Enabled: true})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")

	for _, c := range []struct {
		seat, want int
		empty      bool
	}{{seat: 13, want: 13}, {seat: 0, want: 0, empty: true}, {seat: -2, want: 0, empty: true}} {
		testutil.MustNoErr(t, db.UpdateProcessNodeUOP(nodeID, c.seat), "seat the count")
		written, _, _, found, err := db.ReleaseCountWithCursor(nodeID, nil, protocol.InvDeltaScopeBin, "4402", 1)
		testutil.MustNoErr(t, err, "release with the seat's count")
		rt, err := db.GetProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "read runtime")
		if !found || written != c.want || rt.RemainingUOPCached != c.want || rt.ReleasedEmpty != c.empty {
			t.Errorf("seat %d: found=%t written=%d seat after=%d released_empty=%t, want true %d %d %t",
				c.seat, found, written, rt.RemainingUOPCached, rt.ReleasedEmpty, c.want, c.want, c.empty)
		}
	}
}
