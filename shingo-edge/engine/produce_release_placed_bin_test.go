package engine

import (
	"testing"

	"shingo/protocol/testutil"
)

// produce_release_placed_bin_test.go — the departing produce bin's clear.
//
// Sim 2026-09-07 (PLN_001): under the old press-index shape the index leg
// auto-dispatched at creation and bound its bin through the delivery handler,
// so by the time the operator tapped RELEASE the slot's active bin could be
// the one the press was filling INTO, and the request-time clear erased it.
// A gate in the finalize skipped the clear when the bound bin was the placing
// leg's.
//
// S4 deleted that gate: its state is unreachable. Every press-index leg now
// opens with a station wait, and the finalize runs at the departing leg's
// first release, before either leg moves; the placing leg can bind its bin on
// the press only after the departing leg has lifted the old one, and every
// later finalize of that leg returns at the ledger before the clear. The
// matrix's press-index cells (d1/pi*) pin the finalize at the click. What is
// left here pins the classic clear.

// The classic two_robot shape: the supply leg parks at a staging node and
// never binds here, so the active bin is the DEPARTING one and the clear
// must still fire.
func TestProduceRelease_DepartingBinStillClears(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "RequestProduceSwap")
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	departing := int64(777)
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, nil, 61), "bump count")
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &departing), "bind departing bin")

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "test-op"}), "release")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveBinID != nil {
		t.Errorf("ActiveBinID = %v, want cleared — the classic clear (active bin is the departing one)", rt.ActiveBinID)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 — hold-and-replay window opens at release", rt.RemainingUOPCached)
	}
}
