package engine

import (
	"testing"
)

// TestSwitchNodeToTarget_SkipsUOPResetWhenAlreadyAtTarget verifies the
// Phase-5 idempotent gate on the post-swap confirm: when the operator's
// release click (Phase 3) has already pointed runtime at the to-claim
// and the counter has drifted below capacity while the bots head home,
// SwitchNodeToTarget must NOT clobber that drift back to capacity. This
// is the exact "post-swap confirm" behaviour we're removing.
func TestSwitchNodeToTarget_SkipsUOPResetWhenAlreadyAtTarget(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, toClaimID := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Start the changeover so a node task exists for the switch to advance.
	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Simulate the Phase-3 release click: runtime already points at the
	// to-claim, and a few lineside consumption ticks have drawn the
	// counter down from the to-claim capacity (200) to 137.
	if err := db.SetProcessNodeRuntime(nodeID, &toClaimID, 137); err != nil {
		t.Fatalf("seed runtime at target with drift: %v", err)
	}

	if err := eng.SwitchNodeToTarget(processID, nodeID); err != nil {
		t.Fatalf("switch: %v", err)
	}

	runtime, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	if runtime.RemainingUOP != 137 {
		t.Errorf("RemainingUOP = %d, want 137 (switch must not clobber post-release drift)",
			runtime.RemainingUOP)
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
	if task.State != "switched" {
		t.Errorf("task state = %q, want switched", task.State)
	}
}

// TestSwitchNodeToTarget_ResetsUOPWhenRuntimeStillOnFromClaim verifies
// the legacy / safety-net path: when nothing has advanced runtime to
// the to-claim yet (e.g. an operator uses the admin "SWITCH TO TARGET"
// button without going through the Phase-3 release click), the switch
// still performs the UOP reset so the counter lands at to-claim
// capacity.
func TestSwitchNodeToTarget_ResetsUOPWhenRuntimeStillOnFromClaim(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, fromClaimID, toClaimID := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Runtime still points at the from-claim (legacy path — release
	// click never fired, or this is an admin-driven override).
	if err := db.SetProcessNodeRuntime(nodeID, &fromClaimID, 5); err != nil {
		t.Fatalf("seed runtime on from-claim: %v", err)
	}

	if err := eng.SwitchNodeToTarget(processID, nodeID); err != nil {
		t.Fatalf("switch: %v", err)
	}

	runtime, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	// seedChangeoverScenario's to-claim has UOPCapacity=200.
	if runtime.RemainingUOP != 200 {
		t.Errorf("RemainingUOP = %d, want 200 (to-claim capacity, legacy reset path)",
			runtime.RemainingUOP)
	}
	if runtime.ActiveClaimID == nil || *runtime.ActiveClaimID != toClaimID {
		t.Errorf("ActiveClaimID not pointing at to-claim after switch: %+v", runtime.ActiveClaimID)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.State != "switched" {
		t.Errorf("task state = %q, want switched", task.State)
	}
}
