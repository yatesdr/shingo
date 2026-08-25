//go:build docker

package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// ---------------------------------------------------------------------------
// A refused recovery must be retryable.
//
// recoveryEdgeUUID is deterministic per (bin, robot) — that is what makes a
// racing double-create fail on the edge_uuid unique index instead of putting
// two robots on one bin. But every fail() arm terminalizes the order, and
// liveRecoveryOrderForBin only matches NON-terminal orders. So a refusal left a
// terminal row holding the one uuid this (bin, robot) pair can ever use, the
// in-flight check saw nothing, and the retry died on the unique index.
//
// The refusals are the ORDINARY path, not an exceptional one: slot_unavailable,
// lane_held and fleet_failed are all things a working plant does, and the
// function's own doc comment calls them "a legitimate 'not now', retry later".
// One of them made the bin unrecoverable-by-order for good, leaving a pallet
// jack as the only exit.
// ---------------------------------------------------------------------------

// TestRecoverCarriedBin_RetryAfterAFleetRefusal is the F2 regression.
//
// The fleet refuses, which fails the order; then the fleet recovers and the
// operator presses again. That second press must produce an order.
func TestRecoverCarriedBin_RetryAfterAFleetRefusal(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	dest := &nodes.Node{Name: "DEST-RETRY", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	bin := seedCarried(t, db, "AMR-RETRY", "DEST-RETRY")
	cacheRobot(eng, dispatchableRobot("AMR-RETRY"))

	// The fleet is not taking orders right now.
	backend.SetFail(true)
	if _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err == nil {
		t.Fatal("want a refusal while the fleet is refusing")
	}

	// The fleet comes back, and the operator presses again.
	backend.SetFail(false)
	order, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	if err != nil {
		t.Fatalf("retry after a refusal failed: %v\n"+
			"The refusal terminalized an order holding this (bin, robot) pair's only deterministic "+
			"edge_uuid, and the in-flight check does not see terminal rows — so the retry collides "+
			"with the unique index and the bin can never be recovered by order again.", err)
	}
	if order == nil || order.ID == 0 {
		t.Fatal("retry returned no order")
	}
	if protocol.IsTerminal(order.Status) {
		t.Errorf("retry produced a terminal order (%s) — it must be a live one", order.Status)
	}
	if order.RobotID != "AMR-RETRY" {
		t.Errorf("retry order robot = %q, want AMR-RETRY", order.RobotID)
	}

	// The fleet really was asked this time.
	if len(backend.CreateRequests()) == 0 {
		t.Error("no fleet create request on the successful retry")
	}
}

// TestRecoverCarriedBin_LiveOrderStillBlocksARetry is the guard the clear must
// not weaken. Only a TERMINAL prior may be cleared out of the way; a live one
// still owns the bin, and a second order on one bin is the phantom-delivery
// shape this whole subsystem exists to avoid.
func TestRecoverCarriedBin_LiveOrderStillBlocksARetry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	dest := &nodes.Node{Name: "DEST-LIVE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	bin := seedCarried(t, db, "AMR-LIVE", "DEST-LIVE")
	cacheRobot(eng, dispatchableRobot("AMR-LIVE"))

	first, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "first recovery")

	_, err = eng.RecoverCarriedBin(bin.ID, "operator:test")
	if err == nil {
		t.Fatal("a live recovery order must still refuse a second press")
	}
	if !strings.Contains(err.Error(), "already in flight") {
		t.Errorf("refusal = %q, want it to name the live order", err.Error())
	}

	// And the live order is untouched — nothing cleared its uuid.
	live, err := db.GetOrder(first.ID)
	testutil.MustNoErr(t, err, "re-read the live order")
	if live.EdgeUUID == "" {
		t.Error("the live order's edge_uuid was cleared — only a terminal prior may be cleared, " +
			"and clearing a live one would let a second order be minted for the same bin")
	}
}
