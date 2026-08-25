package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// N1-d — ONE "TOOLING DONE" PER PRESS.
//
// The owner's sentence: the operator does their tool change, marks it done
// ONCE, and the staged material moves in. What shipped instead was three doors
// and none of them that:
//
//   - the changeover-wide release had no HTTP surface at all — the handler was
//     deleted in 2026-08 as an HMI orphan, leaving Engine.ReleaseChangeoverWait
//     reachable only from Go;
//   - the operator board's per-node RELEASE refused these legs, because
//     ResolveSwapPair requires a coordinated two-robot pair and a cleared
//     position's clear-and-refill is ONE order on ONE robot ("order 58 has no
//     sibling — not a coordinated pair");
//   - only per-order release worked, one click per leg, from a screen the
//     operator is not standing at. On the disjoint shape that is four clicks.
//
// Sim 2026-08-24, all three observed with four legs sitting staged.
// ---------------------------------------------------------------------------

// stageEveryLeg forces every leg of the changeover to `staged`, which is where
// a tooling changeover parks: the robots are holding their bins at the staging
// node, waiting for a human to say the setup is finished.
func stageEveryLeg(t *testing.T, db *store.DB, changeoverID int64) []int64 {
	t.Helper()
	tasks, err := db.ListChangeoverNodeTasks(changeoverID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var staged []int64
	for _, task := range tasks {
		for _, id := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if id == nil {
				continue
			}
			testutil.MustNoErr(t, db.UpdateOrderStatus(*id, string(orders.StatusStaged)), "force staged")
			staged = append(staged, *id)
		}
	}
	return staged
}

// TestToolingDoneReleasesEveryStagedLeg is the contract: one call, every leg of
// the press moves.
func TestToolingDoneReleasesEveryStagedLeg(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedMarkedPressScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient("http://test-core")

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "tooling done")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	staged := stageEveryLeg(t, db, co.ID)
	if len(staged) < 2 {
		t.Fatalf("expected a leg per marked position, got %d", len(staged))
	}

	res, err := eng.ReleaseChangeoverWait(processID, ReleaseDisposition{CalledBy: "operator"})
	if err != nil {
		t.Fatalf("tooling-done release: %v", err)
	}
	if res.Released != len(staged) {
		t.Errorf("released %d of %d staged legs (pending %d).\n"+
			"One click means one click: every leg of the press hangs off the same "+
			"tooling-done, or the operator is left hunting the ones that did not move.",
			res.Released, len(staged), res.Pending)
	}
	for _, id := range staged {
		order, err := db.GetOrder(id)
		if err != nil {
			t.Fatalf("get order %d: %v", id, err)
		}
		if order.Status == orders.StatusStaged {
			t.Errorf("order %d is still staged after tooling-done", id)
		}
	}
}

// TestReleaseStagedOrdersAcceptsASingleLegPosition is the operator board's own
// button. A cleared position's leg is one order on one robot — legitimately single
// — and "not a coordinated pair" is a refusal about SWAP pairs that
// over-matched onto it.
func TestReleaseStagedOrdersAcceptsASingleLegPosition(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedMarkedPressScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient("http://test-core")

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "single leg")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	stageEveryLeg(t, db, co.ID)

	position, err := db.GetProcessNodeByCoreNodeName("PRESS-B")
	if err != nil || position == nil {
		t.Fatalf("no node for the paired position: %v", err)
	}
	task, err := db.GetChangeoverNodeTaskByNode(co.ID, position.ID)
	if err != nil {
		t.Fatalf("get position task: %v", err)
	}
	if task.NextMaterialOrderID == nil {
		t.Fatal("position task has no leg to release")
	}

	if err := eng.ReleaseStagedOrders(position.ID, ReleaseDisposition{CalledBy: "operator"}); err != nil {
		t.Fatalf("per-node RELEASE on a cleared position: %v\n"+
			"This is the button on the tile the operator is standing at. A position's leg is a "+
			"single-leg flow by construction — one robot lifts the bin, fetches the "+
			"replacement and holds it — so refusing it for having no sibling refuses the "+
			"only release path that is in front of them.", err)
	}
	order, err := db.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		t.Fatalf("get position leg: %v", err)
	}
	if order.Status == orders.StatusStaged {
		t.Errorf("position leg %d is still staged after the operator released it", order.ID)
	}
}

// A node that is NOT in a changeover keeps the pair path exactly as it was —
// the single-leg acceptance must not become a way for an ordinary swap to
// release half of itself.
func TestReleaseStagedOrdersStillRefusesAnUnpairedSteadyStateNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, _ := seedPhase3SwapScenario(t, db)
	_ = processID
	eng := testEngine(t, db)

	err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "operator"})
	if err == nil {
		t.Error("a steady-state node with no staged pair released anyway — the changeover " +
			"path is for nodes in a changeover, and widening it to every node would let an " +
			"ordinary swap release one leg into a node its sibling has not cleared")
	}
}

// unused-import guard: processes is referenced by the helper signature above.
var _ = processes.NodeTask{}
