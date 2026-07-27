package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestIsPairedOnDeckNode pins the A1 detection query (hop 2026-07-23): a node is
// "on-deck" when a press-index claim in the same process names it as the paired
// (back) or second-paired position. The core/front position is NOT on-deck, an
// unrelated node is not, a blank name is not, and the check is process-scoped.
func TestIsPairedOnDeckNode(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "od.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	processID, err := d.CreateProcess("OD-PROC", "on-deck test", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := d.CreateStyle("OD-STYLE", "od", processID)
	testutil.MustNoErr(t, err, "create style")

	// 3-position press-index: front = PRESS-FRONT (core), back = PRESS-BACK
	// (paired), and PRESS-BACK2 (second paired). Only the two back positions are
	// on-deck.
	_, err = d.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:              styleID,
		CoreNodeName:         "PRESS-FRONT",
		Role:                 "produce",
		SwapMode:             protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:          "WIDGET",
		UOPCapacity:          100,
		PairedCoreNode:       "PRESS-BACK",
		SecondPairedCoreNode: "PRESS-BACK2",
		InboundSource:        "EMPTY",
		OutboundDestination:  "OUT",
	})
	testutil.MustNoErr(t, err, "upsert press-index claim")

	for _, c := range []struct {
		node string
		want bool
	}{
		{"PRESS-BACK", true},   // paired (back) position
		{"PRESS-BACK2", true},  // second paired position
		{"PRESS-FRONT", false}, // the core/front position holds the filling bin
		{"UNRELATED", false},
		{"", false},
	} {
		got, err := d.IsPairedOnDeckNode(processID, c.node)
		testutil.MustNoErr(t, err, "IsPairedOnDeckNode "+c.node)
		if got != c.want {
			t.Errorf("IsPairedOnDeckNode(%q) = %v, want %v", c.node, got, c.want)
		}
	}

	// Process-scoped: PRESS-BACK is not paired in a DIFFERENT process.
	otherProc, err := d.CreateProcess("OD-PROC2", "other", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create other process")
	got, err := d.IsPairedOnDeckNode(otherProc, "PRESS-BACK")
	testutil.MustNoErr(t, err, "IsPairedOnDeckNode other process")
	if got {
		t.Error("IsPairedOnDeckNode must be process-scoped — PRESS-BACK is not paired in the other process")
	}
}
