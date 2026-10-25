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
		&nodeID, false, 1, "CO-NODE", "", "", false, "", "", "")
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
// on the node, the restore's claim advance writes no count either.
//
// CONVERTED (Lane H, DESIGN-H §9.1 / §10 D6): this pinned the old door, where
// the advance wrote 0 over an unbound slot's count. A claim change writes the
// claim only (TestPinH_ClaimAdvanceStatementWritesNoCount); an unbound slot's
// count is zeroed by the departure that unbound it (carrierLeft), not by a
// claim advance. The name is kept so the history reads.
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

	gotRT, err := db.GetProcessNodeRuntime(nodeID)
	rt := testutil.Must(t, gotRT, err, "reload node runtime")
	if rt.RemainingUOPCached != rtBefore.RemainingUOPCached {
		t.Errorf("remaining = %d, want %d unchanged — the claim advance writes the claim, not the count",
			rt.RemainingUOPCached, rtBefore.RemainingUOPCached)
	}
}
