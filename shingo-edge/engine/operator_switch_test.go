package engine

import (
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"testing"
)

// TestSwitchNodeToTarget_SkipsUOPResetWhenAlreadyAtTarget verifies the
// Phase-5 idempotent gate on the post-swap confirm: when the operator's
// release click (Phase 3) has already pointed runtime at the to-claim
// and the counter has drifted below capacity while the bots head home,
// SwitchNodeToTarget must NOT clobber that drift back to capacity. This
// is the exact "post-swap confirm" behaviour we're removing.
func TestSwitchNodeToTarget_SkipsUOPResetWhenAlreadyAtTarget(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, toClaimID := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Start the changeover so a node task exists for the switch to advance.
	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Simulate the Phase-3 release click: runtime already points at the
	// to-claim, and a few lineside consumption ticks have drawn the
	// counter down from the to-claim capacity (200) to 137.
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &toClaimID, 137), "seed runtime at target with drift")

	testutil.MustNoErr(t, eng.SwitchNodeToTarget(processID, nodeID), "switch")

	runtime, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if runtime.RemainingUOPCached != 137 {
		t.Errorf("RemainingUOP = %d, want 137 (switch must not clobber post-release drift)",
			runtime.RemainingUOPCached)
	}
	if runtime.ActiveClaimID == nil || *runtime.ActiveClaimID != toClaimID {
		t.Errorf("ActiveClaimID not pointing at to-claim after switch: %+v", runtime.ActiveClaimID)
	}

	// Node task state still transitions — the Phase-5 guard only skips
	// the UOP reset, not the state bookkeeping.
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.State != domain.NodeTaskSwitched {
		t.Errorf("task state = %q, want switched", task.State)
	}
}

// TestSwitchNodeToTarget_SeedsNoCapacityWhenNoCarrierIsBound verifies the
// safety-net path: when nothing has advanced runtime to the to-claim yet (an
// operator using the admin "SWITCH TO TARGET" button, or a node whose
// changeover delivery never completed), the switch advances the claim and
// leaves the count at zero.
//
// IT USED TO SEED THE TO-CLAIM'S CAPACITY, and this test pinned that. The seed
// was hysteresis — a policy number written so a consume tile would not drop
// below its reorder point during the window between the switch and its bin
// landing — in a field Core reads as a count of parts that physically exist. A
// pinning test is a record of what the code did, not a ruling that it should.
func TestSwitchNodeToTarget_SeedsNoCapacityWhenNoCarrierIsBound(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, fromClaimID, toClaimID := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Runtime still points at the from-claim (legacy path — release
	// click never fired, or this is an admin-driven override).
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &fromClaimID, 5), "seed runtime on from-claim")

	testutil.MustNoErr(t, eng.SwitchNodeToTarget(processID, nodeID), "switch")

	runtime, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	// seedChangeoverScenario's to-claim has UOPCapacity=200, and that number
	// must not appear: no carrier is bound at this node, so there are no parts
	// here and the count says so.
	if runtime.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 — no bin is bound, so any non-zero count is a "+
			"policy value in a measurement field. 200 means the capacity seed came back.",
			runtime.RemainingUOPCached)
	}
	if runtime.ActiveClaimID == nil || *runtime.ActiveClaimID != toClaimID {
		t.Errorf("ActiveClaimID not pointing at to-claim after switch: %+v", runtime.ActiveClaimID)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.State != domain.NodeTaskSwitched {
		t.Errorf("task state = %q, want switched", task.State)
	}
}
