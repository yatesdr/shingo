package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// seedSwapPairAt creates a linked evac/supply pair at a press-index node in
// the given statuses and points the runtime slots at them.
func seedSwapPairAt(t *testing.T, mode protocol.SwapMode, evacStatus, supplyStatus protocol.Status) (*Engine, int64, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, mode, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	eng := testEngine(t, db)

	evacID, err := db.CreateOrder("uuid-evac", orders.TypeComplex, &nodeID, false, 1, "", "", "", "", false, "WIDGET-A")
	testutil.MustNoErr(t, err, "create evac")
	supplyID, err := db.CreateOrder("uuid-supply", orders.TypeComplex, &nodeID, false, 1, "", "", "", "", false, "WIDGET-A")
	testutil.MustNoErr(t, err, "create supply")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(evacStatus)), "evac status")
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(supplyStatus)), "supply status")
	testutil.MustNoErr(t, db.LinkOrderSiblings(evacID, supplyID), "link siblings")
	// ResolveSwapPair reads the runtime slots: Staged -> evac, Active -> supply.
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &supplyID, &evacID), "runtime slots")
	return eng, nodeID, evacID, supplyID
}

func nodeAndClaim(t *testing.T, eng *Engine, nodeID int64) (*processes.Node, *processes.NodeClaim) {
	t.Helper()
	node, _, claim, err := loadActiveNode(eng.db, nodeID)
	testutil.MustNoErr(t, err, "load node")
	return node, claim
}

// THE COLLISION THIS EXISTS TO PREVENT: the supply leg is staged and ready to
// drop a bin on the press, and the evac leg has not lifted the old one off.
// Reachable through the operator's ordinary RELEASE click, because
// ComputeSwapReady shows the button when EITHER leg is staged.
func TestRefusePlacingLegWhileSiblingPending_Refuses(t *testing.T) {
	t.Parallel()
	eng, nodeID, evacID, supplyID := seedSwapPairAt(t,
		protocol.SwapModeTwoRobotPressIndex, protocol.StatusQueued, protocol.StatusStaged)
	node, claim := nodeAndClaim(t, eng, nodeID)

	err := eng.refusePlacingLegWhileSiblingPending(node, claim, &evacID, &supplyID)
	if err == nil {
		t.Fatal("want a refusal: the placing leg would drop onto a press the sibling has not cleared")
	}
	// ADVISORY, not an error. Nothing is broken; the other robot is coming and
	// the operator's only correct action is to click again. Typed rather than
	// matched on the sentence — a reworded message must not turn an all-clear
	// back into a red alarm.
	var advisory interface{ Advisory() bool }
	if !errors.As(err, &advisory) || !advisory.Advisory() {
		t.Errorf("the hold must report itself advisory; got %T", err)
	}
	var notReady *SwapPairNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("want a *SwapPairNotReadyError; got %T", err)
	}
	// It names what is being waited on, so the operator knows when to retry.
	if notReady.SiblingState != string(protocol.StatusQueued) {
		t.Errorf("refusal reports sibling state %q, want %q", notReady.SiblingState, protocol.StatusQueued)
	}
	_ = supplyID
}

// Every case where there is nothing to collide with must pass through — a gate
// that refuses too much strands a supply leg with no sibling that can ever
// stage, which is worse than the collision it prevents.
func TestRefusePlacingLegWhileSiblingPending_LetsEverythingElseThrough(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                     string
		mode                     protocol.SwapMode
		evacStatus, supplyStatus protocol.Status
		nilEvac, nilSupply       bool
	}{
		// The evac already ran, or was cancelled, or was skipped because the
		// press was found empty. Nothing is coming.
		{name: "evac already confirmed", mode: protocol.SwapModeTwoRobotPressIndex,
			evacStatus: protocol.StatusConfirmed, supplyStatus: protocol.StatusStaged},
		{name: "evac cancelled", mode: protocol.SwapModeTwoRobotPressIndex,
			evacStatus: protocol.StatusCancelled, supplyStatus: protocol.StatusStaged},
		{name: "evac skipped", mode: protocol.SwapModeTwoRobotPressIndex,
			evacStatus: protocol.StatusSkipped, supplyStatus: protocol.StatusStaged},
		// Both ready: the ordinary release, which must not be slowed down.
		{name: "both staged", mode: protocol.SwapModeTwoRobotPressIndex,
			evacStatus: protocol.StatusStaged, supplyStatus: protocol.StatusStaged},
		// The placing leg is not going anywhere on this click anyway; the
		// existing per-leg gate handles it and the deferral remembers it.
		{name: "supply not releasable either", mode: protocol.SwapModeTwoRobotPressIndex,
			evacStatus: protocol.StatusQueued, supplyStatus: protocol.StatusQueued},
		// SCOPED TO PRESS-INDEX: two_robot's supply parks at a staging node,
		// not on the press, and its release ordering has been in production
		// unchanged for a long time.
		{name: "two_robot is out of scope", mode: protocol.SwapModeTwoRobot,
			evacStatus: protocol.StatusQueued, supplyStatus: protocol.StatusStaged},
		// One-legged: no sibling to wait for.
		{name: "no evac leg", mode: protocol.SwapModeTwoRobotPressIndex,
			evacStatus: protocol.StatusQueued, supplyStatus: protocol.StatusStaged, nilEvac: true},
		{name: "no supply leg", mode: protocol.SwapModeTwoRobotPressIndex,
			evacStatus: protocol.StatusQueued, supplyStatus: protocol.StatusStaged, nilSupply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, nodeID, evacID, supplyID := seedSwapPairAt(t, tc.mode, tc.evacStatus, tc.supplyStatus)
			node, claim := nodeAndClaim(t, eng, nodeID)
			ep, sp := &evacID, &supplyID
			if tc.nilEvac {
				ep = nil
			}
			if tc.nilSupply {
				sp = nil
			}
			if err := eng.refusePlacingLegWhileSiblingPending(node, claim, ep, sp); err != nil {
				t.Errorf("must not refuse: %v", err)
			}
		})
	}
}

// THE GATE'S OTHER HALF. When the evac IS releasable and the supply is not,
// the evac goes and the supply is remembered — so the pair completes without a
// second click once the supply stages. The gate above must not have broken it.
func TestReleaseStagedOrders_DeferredSiblingStillRemembered(t *testing.T) {
	t.Parallel()
	eng, nodeID, evacID, supplyID := seedSwapPairAt(t,
		protocol.SwapModeTwoRobotPressIndex, protocol.StatusStaged, protocol.StatusQueued)

	if err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "gate-test"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	eng.pendingSiblingReleaseMu.Lock()
	_, remembered := eng.pendingSiblingRelease[supplyID]
	eng.pendingSiblingReleaseMu.Unlock()
	if !remembered {
		t.Error("the deferred supply leg was not remembered — the operator's single click " +
			"expressed 'go' for the whole pair, and deferring is not dropping")
	}
	_ = evacID
}
