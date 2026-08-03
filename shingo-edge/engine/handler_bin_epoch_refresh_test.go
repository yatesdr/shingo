package engine

import (
	"testing"

	"shingo/protocol"
)

// The repair message says one thing and one thing only: "the carrier you are
// holding has started a new generation — adopt the stamp." It carries no count,
// and it must not touch one.
//
// That restraint is the reason it is not a UOP adjustment. An adjustment's
// count field is a plain int with no way to say "absent"; an epoch-only
// adjustment serialises a zero, and the Edge's adjustment handler writes the
// count it is given. Sending the repair that way would zero the count at every
// carrier it repaired — the Edge's own number, the one the operator's screen
// shows — and it fires on the first discarded count after every reset, so it
// would not be rare. The test below is that hazard, pinned.
func TestBinEpochRefresh_AdvancesTheStampAndLeavesTheCountAlone(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "REFRESH-N1", 6001, 4)

	before, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || before == nil {
		t.Fatalf("read runtime: %v", err)
	}

	eng.HandleBinEpochRefresh(protocol.BinEpochRefresh{
		BinID:        binID,
		CoreNodeName: "REFRESH-N1",
		Epoch:        9,
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 9 {
		t.Errorf("epoch = %d, want 9 — the repair exists to move the stamp forward; "+
			"if it does not, the counts this node reports stay discarded", rt.ActiveBinEpoch)
	}
	if rt.RemainingUOPCached != before.RemainingUOPCached {
		t.Errorf("remaining = %d, want %d unchanged — the repair carries no count and must "+
			"not write one; a zero here is the Edge's own number destroyed at every "+
			"carrier the repair touches", rt.RemainingUOPCached, before.RemainingUOPCached)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("active bin = %v, want %d — the repair must not move the carrier pointer", rt.ActiveBinID, binID)
	}
}

// TestBinEpochRefresh_IgnoredWhenTheCarrierIsNotBoundHere: Core sends the
// repair to the station it believes holds the carrier. If this node is holding
// something else, the message is about a carrier that is not here and its stamp
// belongs to nothing at this slot.
func TestBinEpochRefresh_IgnoredWhenTheCarrierIsNotBoundHere(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, _ := boundNodeFixture(t, eng, "REFRESH-N2", 6002, 4)

	eng.HandleBinEpochRefresh(protocol.BinEpochRefresh{
		BinID:        6003, // a different carrier
		CoreNodeName: "REFRESH-N2",
		Epoch:        99,
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 4 {
		t.Errorf("epoch = %d, want the original 4 — the repair named a carrier this node "+
			"is not holding, so its stamp went onto the wrong carrier's counts", rt.ActiveBinEpoch)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 6002 {
		t.Errorf("active bin = %v, want 6002 — the repair must never rebind a slot", rt.ActiveBinID)
	}
}

// TestBinEpochRefresh_NeverWalksTheStampBackwards: the repair rides the same
// reordering channel as everything else, so it inherits the same rule.
func TestBinEpochRefresh_NeverWalksTheStampBackwards(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "REFRESH-N3", 6004, 9)

	eng.HandleBinEpochRefresh(protocol.BinEpochRefresh{
		BinID:        binID,
		CoreNodeName: "REFRESH-N3",
		Epoch:        4,
	})

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 9 {
		t.Errorf("epoch = %d, want 9 — a late repair carrying an older stamp overwrote a newer one", rt.ActiveBinEpoch)
	}
}
