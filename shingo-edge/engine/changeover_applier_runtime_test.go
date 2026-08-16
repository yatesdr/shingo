package engine

import (
	"testing"
)

// The changeover applier must repoint the node's runtime slots at the legs it
// just created. It did not, and ComputeSwapReady — which resolves the evac from
// StagedOrderID — kept answering from the PREVIOUS cycle's pair.
//
// Springfield SNF2 / ALN_001, 2026-08-03: the 21:19 changeover's two legs both
// reached staged, but the runtime row still named the 18:49 pair (whose evac had
// been confirmed 2½ hours earlier), so swap_ready stayed false and the operator
// had no RELEASE button for 13 minutes. Stale pointers don't merely fail to
// help: ResolveSwapPair reaches its task fallback only when BOTH runtime
// pointers are nil, so the stale pair actively shadows the correct one that
// changeover_node_tasks was holding all along.
//
// Hence the pre-poisoning below. A test that starts from a clean runtime row
// would pass against the un-fixed applier for the wrong reason — nil pointers
// let the task fallback rescue the resolution. The regression only shows itself
// when the slots already hold a finished pair, which is the normal state of a
// node between changeovers.
func TestChangeoverApplier_RepointsRuntimeAtNewLegs(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Stand in for the previous cycle: slots occupied by a pair that has
	// already finished.
	stalePrevSupply, stalePrevEvac := int64(90001), int64(90002)
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &stalePrevSupply, &stalePrevEvac); err != nil {
		t.Fatalf("seed stale runtime pointers: %v", err)
	}

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", "runtime repoint"); err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	co, err := db.GetActiveProcessChangeover(processID)
	if err != nil || co == nil {
		t.Fatalf("get active changeover: %v", err)
	}
	tasks, err := db.ListChangeoverNodeTasks(co.ID)
	if err != nil {
		t.Fatalf("list node tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("node tasks = %d, want 1", len(tasks))
	}
	task := tasks[0]
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatalf("swap task should carry both legs: supply=%v evac=%v",
			task.NextMaterialOrderID, task.OldMaterialReleaseOrderID)
	}

	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if rt.ActiveOrderID == nil || rt.StagedOrderID == nil {
		t.Fatalf("runtime slots left empty: active=%v staged=%v", rt.ActiveOrderID, rt.StagedOrderID)
	}
	// active = supply, staged = evac — the positional mapping ResolveSwapPair
	// and the operator-initiated pair-creation sites share.
	if *rt.ActiveOrderID != *task.NextMaterialOrderID {
		t.Errorf("active_order_id = %d, want the new supply leg %d (stale value was %d)",
			*rt.ActiveOrderID, *task.NextMaterialOrderID, stalePrevSupply)
	}
	if *rt.StagedOrderID != *task.OldMaterialReleaseOrderID {
		t.Errorf("staged_order_id = %d, want the new evac leg %d (stale value was %d)",
			*rt.StagedOrderID, *task.OldMaterialReleaseOrderID, stalePrevEvac)
	}
}
