package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

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

// ── HandleUOPAdjustment, site by site ─────────────────────────────────────
//
// S5 makes UOPAdjustment and BinEpochRefresh NoExpiry, so an announcement can
// now arrive hours late, behind newer ones. That is safe only if no write
// reached from either handler can walk a carrier's stamp backward. Each
// HandleUOPAdjustment branch that writes an epoch is pinned here: the Bound
// bind, the bind of a staged carrier into an empty slot, and the count
// correction of the carrier already bound. All three write through
// epochAssignOnBind: for the same bound bin the stamp only moves forward. The
// empty-slot bind is the exception at base, because an empty slot has no bound
// bin to compare with; its pins are below.

// adjustmentEpoch applies adj and returns the node's runtime afterwards.
func adjustmentEpoch(t *testing.T, eng *Engine, node int64, adj protocol.UOPAdjustment) *processes.RuntimeState {
	t.Helper()
	eng.HandleUOPAdjustment(adj)
	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	return rt
}

// TestUOPAdjustmentEpoch_BoundSiteNeverGoesBackward: the Bound branch (Core
// moved the bin onto this node) names a bin already bound here at a newer
// generation. The stale announcement lands its count, not its stamp.
func TestUOPAdjustmentEpoch_BoundSiteNeverGoesBackward(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "EPOCH-SITE-BOUND", 7101, 9)

	rt := adjustmentEpoch(t, eng, node, protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: "EPOCH-SITE-BOUND", NewRemaining: 11, Epoch: 4,
		Bound: true, Actor: "admin-under-test",
	})
	if rt.ActiveBinEpoch != 9 {
		t.Errorf("epoch = %d, want 9 — a late Bound announcement walked the stamp backward", rt.ActiveBinEpoch)
	}
	if rt.RemainingUOPCached != 11 {
		t.Errorf("remaining = %d, want 11 — the Bound write ran; only its stamp is refused", rt.RemainingUOPCached)
	}
}

// TestUOPAdjustmentEpoch_CorrectionSiteNeverGoesBackward: the count correction
// of the bin bound here (neither Bound nor Released), carrying an older stamp.
func TestUOPAdjustmentEpoch_CorrectionSiteNeverGoesBackward(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "EPOCH-SITE-CORR", 7102, 9)

	rt := adjustmentEpoch(t, eng, node, protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: "EPOCH-SITE-CORR", NewRemaining: 11, Epoch: 4,
		Actor: "admin-under-test",
	})
	if rt.ActiveBinEpoch != 9 {
		t.Errorf("epoch = %d, want 9 — a late correction walked the stamp backward", rt.ActiveBinEpoch)
	}
	if rt.RemainingUOPCached != 11 {
		t.Errorf("remaining = %d, want 11 — the guard is on the stamp, the count still lands", rt.RemainingUOPCached)
	}
}

// departedSlotFixture binds bin X at epoch 4, moves it on to epoch 5 (Core
// cleared it for reuse and the station adopted the new stamp), then empties the
// slot the way Core's admin Move does (a Released adjustment). The slot's
// active_bin_epoch still reads 5 afterwards, but the row no longer says whose
// 5 it is.
func departedSlotFixture(t *testing.T, eng *Engine, coreNode string, binID int64) int64 {
	t.Helper()
	node, _ := boundNodeFixture(t, eng, coreNode, binID, 4)
	eng.HandleBinEpochRefresh(protocol.BinEpochRefresh{BinID: binID, CoreNodeName: coreNode, Epoch: 5})
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: coreNode, Released: true, Epoch: 5, Actor: "admin-under-test",
	})
	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinID != nil || rt.ActiveBinEpoch != 5 {
		t.Fatalf("fixture: active bin = %v epoch = %d, want an empty slot whose stamp still reads 5",
			rt.ActiveBinID, rt.ActiveBinEpoch)
	}
	return node
}

// TestEmptySlotBind_RefusesADepartedCarriersOldStamp is the empty-slot guard
// (the S5a precondition). Bin X was bound at 4, moved on to 5, then left the
// slot. A late correction for X at 4 must not rebind the carrier that left or
// stamp it with a generation that has ended. The slot remembers X at 5 from the
// statement that emptied it, and the bind is refused: the slot stays empty, its
// stamp and its blanked count unchanged.
//
// Inverted pin: at base
// (TestPin_EmptySlotBindOfADepartedCarrierTakesItsOldStamp) the correction
// rebound X at 4.
func TestEmptySlotBind_RefusesADepartedCarriersOldStamp(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	const binX = int64(7103)
	node := departedSlotFixture(t, eng, "EPOCH-SITE-EMPTY", binX)

	rt := adjustmentEpoch(t, eng, node, protocol.UOPAdjustment{
		BinID: binX, CoreNodeName: "EPOCH-SITE-EMPTY", NewRemaining: 11, Epoch: 4,
		Actor: "admin-under-test",
	})
	if rt.ActiveBinID != nil {
		t.Fatalf("active bin = %d, want none — a late correction rebound a carrier that left", *rt.ActiveBinID)
	}
	if rt.ActiveBinEpoch != 5 {
		t.Errorf("epoch = %d, want 5 — the refused bind moved the stamp", rt.ActiveBinEpoch)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("remaining = %d, want 0 — the refused bind wrote its count", rt.RemainingUOPCached)
	}
}

// TestEmptySlotBind_ADifferentCarrierBindsAtItsOwnStamp: the empty-slot bind
// is a real repair for a staged carrier the Edge never bound. A different bin
// binds at its own stamp however old the departed bin's was. Green before and
// after the guard.
func TestEmptySlotBind_ADifferentCarrierBindsAtItsOwnStamp(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node := departedSlotFixture(t, eng, "EPOCH-SITE-OTHER", 7105)

	const binY = int64(7106)
	rt := adjustmentEpoch(t, eng, node, protocol.UOPAdjustment{
		BinID: binY, CoreNodeName: "EPOCH-SITE-OTHER", NewRemaining: 11, Epoch: 2,
		Actor: "admin-under-test",
	})
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binY || rt.ActiveBinEpoch != 2 {
		t.Errorf("active bin = %v epoch = %d, want %d at 2", rt.ActiveBinID, rt.ActiveBinEpoch, binY)
	}
}

// TestEmptySlotBind_TheSameCarrierAtItsCurrentStampBinds: the carrier that
// left, corrected at the stamp the slot last held, binds. An equal stamp is
// not older. Green before and after the guard.
func TestEmptySlotBind_TheSameCarrierAtItsCurrentStampBinds(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	const binX = int64(7107)
	node := departedSlotFixture(t, eng, "EPOCH-SITE-SAME", binX)

	rt := adjustmentEpoch(t, eng, node, protocol.UOPAdjustment{
		BinID: binX, CoreNodeName: "EPOCH-SITE-SAME", NewRemaining: 11, Epoch: 5,
		Actor: "admin-under-test",
	})
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binX || rt.ActiveBinEpoch != 5 {
		t.Errorf("active bin = %v epoch = %d, want %d at 5", rt.ActiveBinID, rt.ActiveBinEpoch, binX)
	}
}

// TestBinEpochRefreshEpoch_NeverGoesBackward: the other announcement S5 makes
// NoExpiry. A refresh that lost a race to a newer stamp leaves the newer one.
func TestBinEpochRefreshEpoch_NeverGoesBackward(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	node, binID := boundNodeFixture(t, eng, "EPOCH-SITE-REFRESH", 7104, 9)

	eng.HandleBinEpochRefresh(protocol.BinEpochRefresh{BinID: binID, CoreNodeName: "EPOCH-SITE-REFRESH", Epoch: 4})
	rt, err := eng.db.GetProcessNodeRuntime(node)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveBinEpoch != 9 {
		t.Errorf("epoch = %d, want 9 — a late refresh walked the stamp backward", rt.ActiveBinEpoch)
	}
}
