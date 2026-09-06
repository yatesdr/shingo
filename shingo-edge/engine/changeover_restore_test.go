package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// The restore path advances a node task whose staging order went terminal
// while Edge was down, and re-points the runtime at the incoming style's
// claim. The question these two pin is what happens to the COUNT on the way.
//
// A restart is not a material event. Whatever is standing on the node was
// standing on it a moment ago, and the number beside it was a measurement.

func seedStagingRestoreTask(t *testing.T, db *store.DB) (nodeID, toStyleID, toClaimID int64, task *processes.NodeTask) {
	t.Helper()
	_, nodeID, _, toStyleID, _, toClaimID = seedChangeoverScenario(t, db)

	orderID, err := db.CreateOrder("uuid-restore-staging", orders.TypeRetrieve,
		&nodeID, false, 1, "CO-NODE", "", "", "", false, "")
	testutil.MustNoErr(t, err, "create staging order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "terminal staging order")

	task = &processes.NodeTask{
		ID:                  1,
		ProcessNodeID:       nodeID,
		State:               domain.NodeTaskStagingRequested,
		NextMaterialOrderID: &orderID,
	}
	return nodeID, toStyleID, toClaimID, task
}

// TestRestore_BoundCarrierKeepsItsCount: the staging order going terminal is
// exactly the case where the bin was DELIVERED and its count seeded, so this
// arm fires with a real carrier standing on the node. Zeroing it there tells
// the line a full node is starved, and the next PLC tick ships that to Core.
func TestRestore_BoundCarrierKeepsItsCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, toStyleID, toClaimID, task := seedStagingRestoreTask(t, db)

	bin := int64(771)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &toClaimID, &bin, 3, 175),
		"bind a carrier with a measured count")

	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})

	if !eng.reconcileNodeTask(task, toStyleID) {
		t.Fatal("reconcileNodeTask did not advance the staging task")
	}

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.RemainingUOPCached != 175 {
		t.Errorf("remaining = %d, want 175 — a restart is not a material event, and bin %d is "+
			"still standing on the node with 175 parts somebody counted", rt.RemainingUOPCached, bin)
	}
}

// TestRestore_SeedsZeroWhenNoCarrierIsBound is the other half: with no carrier
// on the node there is no measurement to keep, and 0 is what is there.
func TestRestore_SeedsZeroWhenNoCarrierIsBound(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, toStyleID, _, task := seedStagingRestoreTask(t, db)

	rtBefore, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil || rtBefore == nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rtBefore.ActiveBinID != nil {
		t.Fatalf("fixture bound a carrier (%d); this test needs an empty node", *rtBefore.ActiveBinID)
	}

	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})

	if !eng.reconcileNodeTask(task, toStyleID) {
		t.Fatal("reconcileNodeTask did not advance the staging task")
	}

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 0 {
		t.Errorf("remaining = %d, want 0 — nothing is bound to this node, so there is no count "+
			"to carry over from the outgoing style", rt.RemainingUOPCached)
	}
}
