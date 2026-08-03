package engine

import (
	"testing"

	"shingo/protocol"
)

// TestUOPAdjustment_CountCorrectionTakesTheEpochToo closes the count-loss found
// at Hopkinsville on 2026-08-02.
//
// Clearing a bin for reuse on Core's admin screen sends this message: neither
// Bound nor Released, a new count, and a NEW EPOCH. The Edge took the count and
// dropped the epoch — accepting "this bin now holds N" while ignoring "...and it
// has started a new life". Every delta it sent afterwards carried a stale stamp
// and Core discarded it. 19,245 counts dropped against 19,962 applied in 30
// days; on one day 3,200 dropped and none landed.
//
// The epoch is safe to adopt HERE specifically because it rides the count: the
// handler has already established this bin is the one bound at this node, and
// the same message resets the count, so no old-life ticks can be misapplied to a
// new life. Taking one half and not the other was the defect.
func TestUOPAdjustment_CountCorrectionTakesTheEpochToo(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "EPOCH-N1", 4242, 7)

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "EPOCH-N1",
		NewRemaining: 0,
		Epoch:        99, // Core cleared the bin: new life, new stamp
		Actor:        "admin-under-test",
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("remaining = %d, want 0 — the count half already worked and must keep working", rt.RemainingUOPCached)
	}
	if rt.ActiveBinEpoch != 99 {
		t.Errorf("epoch = %d, want 99 — Core sent the new stamp with the new count and the Edge kept the old one; "+
			"every delta after this carries a stale stamp and Core throws it away", rt.ActiveBinEpoch)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("active bin = %v, want %d — taking the epoch must not unbind the carrier", rt.ActiveBinID, binID)
	}
}

// TestUOPAdjustment_OlderCoreKeepsItsEpoch: a Core that predates the epoch field
// sends zero. Adopting that would blank the stamp and make every subsequent
// delta look like it came from before the bin's first life.
func TestUOPAdjustment_OlderCoreKeepsItsEpoch(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "EPOCH-N2", 5150, 12)

	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        binID,
		CoreNodeName: "EPOCH-N2",
		NewRemaining: 3,
		Epoch:        0, // older Core: says nothing about the epoch
		Actor:        "admin-under-test",
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 12 {
		t.Errorf("epoch = %d, want the original 12 — a silent older Core must not blank the stamp", rt.ActiveBinEpoch)
	}
	if rt.RemainingUOPCached != 3 {
		t.Errorf("remaining = %d, want 3 — the count still applies", rt.RemainingUOPCached)
	}
}

// boundNodeFixture creates a process node with a bin bound at a known epoch, so
// a test can watch what an adjustment does to it. Returns the node id.
func boundNodeFixture(t *testing.T, eng *Engine, coreNode string, binID, epoch int64) (int64, int64) {
	t.Helper()
	procID, err := eng.db.CreateProcess("EPOCH-PROC-"+coreNode, "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	seedWindowNodes(t, eng.db, "EPOCH-SEED-"+coreNode, []string{coreNode})
	_ = procID
	n, err := eng.db.GetProcessNodeByCoreNodeName(coreNode)
	if err != nil || n == nil {
		t.Fatalf("resolve seeded node %s: %v", coreNode, err)
	}
	rt, err := eng.db.EnsureProcessNodeRuntime(n.ID)
	if err != nil || rt == nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	b := binID
	if err := eng.db.SetProcessNodeRuntimeWithBinAndEpoch(n.ID, rt.ActiveClaimID, &b, epoch, 25); err != nil {
		t.Fatalf("bind bin at epoch: %v", err)
	}
	return n.ID, binID
}
