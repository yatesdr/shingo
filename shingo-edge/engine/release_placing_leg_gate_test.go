package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/release"
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

	evacID, err := db.CreateOrder("uuid-evac", orders.TypeComplex, &nodeID, false, 1, "", "", "", "", false, "WIDGET-A", "", "")
	testutil.MustNoErr(t, err, "create evac")
	supplyID, err := db.CreateOrder("uuid-supply", orders.TypeComplex, &nodeID, false, 1, "", "", "", "", false, "WIDGET-A", "", "")
	testutil.MustNoErr(t, err, "create supply")
	// The evac's steps say what it is: it waits at the press, then lifts the
	// press's bin — the leg a RELEASE finalizes the produce bin for.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID,
		`[{"action":"wait","node":"PRESS","wait_kind":"station"},{"action":"pickup","node":"PRESS"},{"action":"dropoff","node":"OUT"}]`),
		"evac steps")
	// The supply is R2: it waits at the paired position, lifts that carrier
	// and sets it on the press — the drop the evac must clear first.
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(supplyID,
		`[{"action":"wait","node":"PRESS-B","wait_kind":"station","purpose":"swap"},{"action":"pickup","node":"PRESS-B"},{"action":"dropoff","node":"PRESS"}]`),
		"supply steps")
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

// When the evac IS releasable and the supply is not, the evac goes and the
// supply is remembered — a held intent, so the pair completes without a second
// click once the supply stages (S5; the in-memory deferral before).
func TestReleaseStagedOrders_DeferredSiblingStillRemembered(t *testing.T) {
	t.Parallel()
	eng, nodeID, evacID, supplyID := seedSwapPairAt(t,
		protocol.SwapModeTwoRobotPressIndex, protocol.StatusStaged, protocol.StatusQueued)

	if err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "gate-test"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	o, err := eng.db.GetOrder(supplyID)
	testutil.MustNoErr(t, err, "read supply")
	in, err := release.DecodeIntent(o.ReleaseIntent)
	testutil.MustNoErr(t, err, "decode intent")
	remembered := in != nil && !in.Sent()
	if !remembered {
		t.Error("the deferred supply leg was not remembered — the operator's single click " +
			"expressed 'go' for the whole pair, and deferring is not dropping")
	}
	_ = evacID
}
