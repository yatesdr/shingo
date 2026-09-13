package engine

import (
	"testing"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// changeover_drain_state_test.go — what the runtime can honestly say about the
// bin at a press position.
//
// THE CASE THIS EXISTS FOR. A press whose counter tag is not wired reads
// RemainingUOPCached == 0 forever — that is Springfield today, and
// produce_plan.go's partial-empty prime branch is ordered around it. Asked as a
// bool, "is the bin drained" answered yes at every position of such a press,
// and the reuse-compatible-bins shortcut would have skipped every same-part
// swap there. The per-claim opt-in was the only thing keeping that theoretical;
// with the shortcut baked in, the predicate has to tell "the counter says zero"
// from "there is no counter" itself.

func seedDrainStateProcess(t *testing.T, db *store.DB, plcName, tagName string, counterEnabled bool) (processID, nodeID, claimID int64) {
	t.Helper()
	var err error
	processID, err = db.CreateProcess("DRAIN-PROC", "", "active_production", plcName, tagName, counterEnabled)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PLN_01", Code: "pln", Name: "PLN 01", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("DRAIN-STYLE", "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	claimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN_01", Role: "produce", SwapMode: "two_robot_press_index",
		PayloadCode: "PART-A", InboundSource: "SMN", OutboundDestination: "SMN", PairedCoreNode: "PLN_02",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	return processID, nodeID, claimID
}

func TestBinDrainState_UnwiredCounterIsUnknownNotDrained(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	// No PLC name, no tag: nothing polls this press, so RemainingUOPCached is
	// 0 because it has never been anything else.
	processID, nodeID, claimID := seedDrainStateProcess(t, db, "", "", false)
	bin := int64(1)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 0)

	eng := testEngine(t, db)
	if got := eng.binDrainedAtCoreNode(processID)("PLN_01"); got != DrainUnknown {
		t.Errorf("drain state at an unwired press = %v, want unknown — a counter that was never wired is not a counter reading zero", got)
	}
}

func TestBinDrainState_CounterEnabledButNoTagIsUnknown(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	// The checkbox is on, but there is no tag behind it. SyncProcessCounter
	// creates no reporting point in this state, so nothing polls.
	processID, nodeID, claimID := seedDrainStateProcess(t, db, "PLC-1", "", true)
	bin := int64(1)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 0)

	eng := testEngine(t, db)
	if got := eng.binDrainedAtCoreNode(processID)("PLN_01"); got != DrainUnknown {
		t.Errorf("drain state with a half-configured counter = %v, want unknown", got)
	}
}

func TestBinDrainState_WiredCounterAnswersDrainedAndNotDrained(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, claimID := seedDrainStateProcess(t, db, "PLC-1", "TAG-1", true)
	eng := testEngine(t, db)
	bin := int64(1)

	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 0)
	if got := eng.binDrainedAtCoreNode(processID)("PLN_01"); got != Drained {
		t.Errorf("wired counter reading 0 = %v, want drained", got)
	}

	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 5)
	if got := eng.binDrainedAtCoreNode(processID)("PLN_01"); got != NotDrained {
		t.Errorf("wired counter reading 5 = %v, want not drained", got)
	}
}

func TestBinDrainState_NoBinBoundIsUnknown(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, claimID := seedDrainStateProcess(t, db, "PLC-1", "TAG-1", true)
	// Counter wired and reading zero, but no bin at the position: there is no
	// bin whose drain state could be reported, and reusing what is not there
	// is not a shortcut.
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 0)

	eng := testEngine(t, db)
	if got := eng.binDrainedAtCoreNode(processID)("PLN_01"); got != DrainUnknown {
		t.Errorf("drain state with no bin bound = %v, want unknown", got)
	}
}

func TestBinDrainState_UnknownNodeIsUnknown(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _ := seedDrainStateProcess(t, db, "PLC-1", "TAG-1", true)
	eng := testEngine(t, db)
	if got := eng.binDrainedAtCoreNode(processID)("NOT-A-NODE"); got != DrainUnknown {
		t.Errorf("drain state at a node with no process_nodes row = %v, want unknown", got)
	}
}
