package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// TestSwitchNodeToTarget_DropAdvancesWithoutTargetClaim pins the drop
// branch of SwitchNodeToTarget. Pre-fix, the function did an
// unconditional GetStyleNodeClaimByNode against the target style and
// errored "target style claim not found for node" for any drop — the
// new style has no claim there by design. That blocked both the
// per-node Skip button (on /changeover) and the Complete Station
// rollup (which iterates nodes and used to bail on the first error).
// Post-fix: drop tasks advance to line_cleared (terminal-for-drop per
// IsNodeTaskStateTerminal) without touching runtime, and the call
// returns nil.
func TestSwitchNodeToTarget_DropAdvancesWithoutTargetClaim(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedDropScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "drop test")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	// Seed the task into a non-terminal state so the test exercises the
	// switch's job of advancing it. seedDropScenario's claim has no
	// EvacuateOnChangeover marker, so the planner stamps line_cleared
	// at plan time — overwrite that here to simulate the
	// EvacuateOnChangeover=true shape where switch is the action that
	// finishes the task.
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if err := db.UpdateChangeoverNodeTaskState(task.ID, "empty_requested"); err != nil {
		t.Fatalf("seed task to non-terminal: %v", err)
	}

	if err := eng.SwitchNodeToTarget(processID, nodeID); err != nil {
		t.Fatalf("switch on drop node: unexpected error %v", err)
	}

	task, err = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task after switch: %v", err)
	}
	if task.State != "line_cleared" {
		t.Errorf("drop switch task state = %q, want line_cleared", task.State)
	}
}

// TestHandleNodeOrderFailed_DropNoBinAutoClears pins the auto-skip
// branch: when a drop task's evac order fails with Core's no_bin
// shape (bins were present at the pickup but none claimable — e.g.
// payload mismatch, orphan claim from a prior changeover), Edge
// advances the task to line_cleared with a skip_note instead of
// stamping error. Plant 2026-05-12 (SNF2 ALN_002): the failed evac
// stranded the operator at an "error" tile that required the
// /changeover supervisor page to clear; the changeover's intent — get
// the bin off the line — was effectively satisfied either way.
func TestHandleNodeOrderFailed_DropNoBinAutoClears(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedDropScenarioEvacuate(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, err := eng.StartProcessChangeover(processID, toStyleID, "test", "drop no_bin test")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.Situation != "drop" {
		t.Fatalf("expected drop situation, got %s", task.Situation)
	}
	// EvacuateOnChangeover=true on the from-claim → planner lands the
	// task at empty_requested (non-terminal) so cutover waits for the
	// physical pickup. That's the precondition for this regression.
	if task.State != "empty_requested" {
		t.Fatalf("expected empty_requested at plan time (EvacuateOnChangeover), got %s", task.State)
	}
	if task.OldMaterialReleaseOrderID == nil {
		t.Fatal("expected evac order on drop task")
	}
	evacOrderID := *task.OldMaterialReleaseOrderID

	// Simulate Core's no_bin failure for the evac order. Reason
	// matches the wire shape from complex_claims.go:141.
	order, _ := db.GetOrder(evacOrderID)
	emitOrderFailed(eng, evacOrderID, order.UUID, protocol.OrderType("complex"),
		"no available bin at pickup node(s) for order "+itoa(evacOrderID))

	task, err = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task after fail: %v", err)
	}
	if task.State != "line_cleared" {
		t.Errorf("drop+no_bin task state = %q, want line_cleared (auto-clear)", task.State)
	}
	if task.SkipNote == "" {
		t.Errorf("drop+no_bin auto-clear left SkipNote empty; expected operator chip text")
	}
}

// TestHandleNodeOrderFailed_DropOtherFailureStillErrors guards against
// over-broad auto-skip: a drop task whose evac fails for a reason
// that isn't a bin-claim shape (fleet error, invalid steps, etc.)
// must still land at "error" so the operator sees the alarm. Without
// this guard the auto-skip would silently hide real failures.
func TestHandleNodeOrderFailed_DropOtherFailureStillErrors(t *testing.T) {
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedDropScenarioEvacuate(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := eng.StartProcessChangeover(processID, toStyleID, "test", "drop fleet error test")

	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	evacOrderID := *task.OldMaterialReleaseOrderID
	order, _ := db.GetOrder(evacOrderID)

	emitOrderFailed(eng, evacOrderID, order.UUID, protocol.OrderType("complex"),
		"fleet rejected staged order: vendor timeout")

	task, _ = db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.State != "error" {
		t.Errorf("drop+fleet_error task state = %q, want error (auto-skip must not swallow non-bin failures)", task.State)
	}
}

// seedDropScenarioEvacuate mirrors seedDropScenario but sets
// EvacuateOnChangeover=true on the from-claim so the planner lands the
// drop task at empty_requested (non-terminal) rather than line_cleared.
// This is the precondition for the no_bin auto-clear regression: the
// task must be non-terminal at the moment the order fails.
func seedDropScenarioEvacuate(t *testing.T, db *store.DB) (processID, nodeID, fromStyleID, toStyleID int64) {
	t.Helper()
	processID, err := db.CreateProcess("DROPE-PROC", "drop evac test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "DROPE-NODE", Code: "DE1", Name: "Drop Evac Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	fromStyleID, _ = db.CreateStyle("DROPE-FROM", "drop evac from", processID)
	toStyleID, _ = db.CreateStyle("DROPE-TO", "drop evac to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	fcID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "DROPE-NODE", Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-DROP", UOPCapacity: 100, InboundSource: "SRC-DROP",
		OutboundStaging: "OUT-STAGE", OutboundDestination: "DEST-DROP",
		EvacuateOnChangeover: true,
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	db.SetProcessNodeRuntime(nodeID, &fcID, 50)
	return
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
