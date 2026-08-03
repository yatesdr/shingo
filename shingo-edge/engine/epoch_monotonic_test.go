package engine

import "testing"

// The Edge's active_bin_epoch is a copy of Core's bins.delta_epoch. Until now
// it was written last-write-wins by five separate paths, which is only safe
// while messages arrive in the order Core sent them. They do not: the outbox
// drainer publishes in id order but retries a failed message in place, so one
// failure reorders everything behind it. A message that lost a race then stamps
// an OLD generation over a new one, and every count the Edge reports afterwards
// is discarded by Core as stale.
//
// The rule: for the SAME bin the epoch only ever goes forward. When the bin at
// the slot changes, any epoch binds — each bin owns its own generation counter,
// so bin B at 4 is not "older" than bin A at 9, it is unrelated.

func TestActiveBinEpoch_NeverGoesBackwardForTheSameBin(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "EPOCH-MONO-1", 7001, 9)

	// The same bin, an older stamp — a reordered retry off the drainer.
	if err := eng.db.SetProcessNodeRuntimeForDeliveredBin(node, nil, binID, 4, 25); err != nil {
		t.Fatalf("write delivered bin at the older epoch: %v", err)
	}

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 9 {
		t.Errorf("epoch = %d, want 9 — an out-of-order message walked the stamp backwards; "+
			"every delta reported after this carries a stale stamp and Core throws it away", rt.ActiveBinEpoch)
	}
}

func TestActiveBinEpoch_ADifferentBinBindsAtAnyEpoch(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, _ := boundNodeFixture(t, eng, "EPOCH-MONO-2", 7002, 9)

	// A different carrier arrives at the same slot, early in its own life.
	const otherBin = int64(7003)
	if err := eng.db.SetProcessNodeRuntimeForDeliveredBin(node, nil, otherBin, 4, 25); err != nil {
		t.Fatalf("write the arriving bin: %v", err)
	}

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 4 {
		t.Errorf("epoch = %d, want 4 — the guard is over-strict: it compared a new bin's "+
			"generation against the departed bin's and refused it, so the arriving carrier "+
			"reports under a stamp that was never its own", rt.ActiveBinEpoch)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != otherBin {
		t.Errorf("active bin = %v, want %d", rt.ActiveBinID, otherBin)
	}
}

// TestActiveBinEpoch_ZeroIsSimplyNotGreater is the fold: the handler's
// "adj.Epoch > 0" special case (an older Core that sends no epoch must not
// blank the stamp) is not a special case at all once the column only moves
// forward. Zero loses the comparison like any other older value.
func TestActiveBinEpoch_ZeroIsSimplyNotGreater(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "EPOCH-MONO-3", 7004, 12)

	if err := eng.db.SetProcessNodeRuntimeWithBinAndEpoch(node, nil, &binID, 0, 3); err != nil {
		t.Fatalf("write with a silent older Core's zero epoch: %v", err)
	}

	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 12 {
		t.Errorf("epoch = %d, want 12 — a silent older Core blanked the stamp", rt.ActiveBinEpoch)
	}
	if rt.RemainingUOPCached != 3 {
		t.Errorf("remaining = %d, want 3 — refusing the older stamp must not refuse the count", rt.RemainingUOPCached)
	}
}
