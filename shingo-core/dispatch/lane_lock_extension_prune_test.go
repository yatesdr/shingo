//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// TestLaneLockExtension_PrunesAndReleasesForATerminalParent pins the only reason
// boot-time recovery still exists.
//
// The terminal handler consumes a listener row when its parent is cancelled or
// fails. A crash BETWEEN those two moments leaves a row whose parent will never
// pick up — and since the row is what holds the lane open, nothing else would
// ever clear it. The lane would stay held forever, which is the failure the old
// in-memory design had and the reason this sweep survived the row migration.
//
// IT ASSERTS THE ROW, NOT THE LANE, and the reason is a finding rather than a
// simplification. A first version also asserted the lane was released and that
// assertion was VACUOUS: terminalizing an order runs ReleaseByOrder, which is
// order-keyed and kind-agnostic, so it already dropped the dig mouth row along
// with every other reservation. Measured — a probe showed the lane unheld
// BEFORE the prune ran. The Unlock inside the terminal handler is therefore
// belt-and-braces (idempotent, and correct if it is ever reached before
// terminalization); the load-bearing work is deleting the listener row, which
// lives in a table ReleaseByOrder does not touch.
//
// MUTATION (verified): skip the prune for terminal parents. The row survives
// boot and this test's own assertion fires. No checker involved — the
// dispatcher has none.
func TestLaneLockExtension_PrunesAndReleasesForATerminalParent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-PRUNE", 3)
	_ = lane

	parent := testdb.CreateOrder(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// The reshuffle holds the lane, and a listener is armed to release it when
	// the parent finally picks up.
	if !d.laneLock.TryLock(lane, parent.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}
	if _, err := db.InsertPendingLaneExtension(&store.PendingLaneExtension{
		ComplexParentID: parent.ID, LaneID: lane, TargetBinID: 4242, ExpectedFromNodeID: 0,
	}); err != nil {
		t.Fatalf("InsertPendingLaneExtension: %v", err)
	}

	// The parent dies while Core is down, so the terminal handler never ran.
	if err := db.FailOrderAtomic(parent.ID, "died while core was down"); err != nil {
		t.Fatalf("FailOrderAtomic: %v", err)
	}
	reloaded, err := db.GetOrder(parent.ID)
	if err != nil || !protocol.IsTerminal(reloaded.Status) {
		t.Fatalf("parent should be terminal, got %v (%v)", reloaded.Status, err)
	}

	// Boot.
	if err := d.RecoverPendingLaneExtensions(); err != nil {
		t.Fatalf("RecoverPendingLaneExtensions: %v", err)
	}

	if _, err := db.GetPendingLaneExtensionByComplexParent(parent.ID); err == nil {
		t.Error("the listener row for a terminal parent survived boot — it can never fire")
	}
}

// TestLaneLockExtension_LeavesALiveParentAlone is the control: the sweep must
// prune only what can never fire.
//
// MUTATION (verified): invert the IsTerminal check so live parents are pruned
// instead. Both of this test's assertions fire — the row is gone and the lane
// released out from under a reshuffle that is still working.
//
// Note the asymmetry with the terminal case above, which is real: HERE the lane
// assertion has teeth, because a LIVE parent was never terminalized and so
// ReleaseByOrder never ran. Same two lines, load-bearing in one test and
// vacuous in the other, purely because of what terminalization already does.
func TestLaneLockExtension_LeavesALiveParentAlone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-PRUNE-LIVE", 3)

	parent := &orders.Order{
		EdgeUUID: "prune-live", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: protocol.StatusReshuffling, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create live parent")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	if !d.laneLock.TryLock(lane, parent.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}
	if _, err := db.InsertPendingLaneExtension(&store.PendingLaneExtension{
		ComplexParentID: parent.ID, LaneID: lane, TargetBinID: 777, ExpectedFromNodeID: 0,
	}); err != nil {
		t.Fatalf("InsertPendingLaneExtension: %v", err)
	}

	if err := d.RecoverPendingLaneExtensions(); err != nil {
		t.Fatalf("RecoverPendingLaneExtensions: %v", err)
	}

	if _, err := db.GetPendingLaneExtensionByComplexParent(parent.ID); err != nil {
		t.Errorf("the listener for a LIVE parent was pruned: %v", err)
	}
	if !d.laneLock.IsLocked(lane) {
		t.Error("the lane was released out from under a reshuffle that is still working")
	}
}
