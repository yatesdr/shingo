package service

import (
	"testing"

	"shingo/protocol"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// linesideProcess seeds a process with an ACTIVE style, one node, and one
// active-style CONSUME claim on that node. activePayloadLineside walks every
// active consume claim on the edge regardless of process, so tests use two of
// these to exercise the cross-process sum.
func linesideProcess(t *testing.T, db *store.DB, procName, coreNode, code string, payloads []string) (styleID, nodeID int64) {
	t.Helper()
	processID, err := db.CreateProcess(procName, "lineside", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process %s: %v", procName, err)
	}
	styleID, err = db.CreateStyle("STYLE-"+procName, "", processID)
	if err != nil {
		t.Fatalf("create style for %s: %v", procName, err)
	}
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style for %s: %v", procName, err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: coreNode, Code: code, Name: coreNode, Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node %s: %v", coreNode, err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        coreNode,
		Role:                protocol.ClaimRoleConsume,
		SwapMode:            protocol.SwapModeManualSwap,
		OutboundDestination: "MARKET", // manual_swap claims require this
		AllowedPayloadCodes: payloads,
	}); err != nil {
		t.Fatalf("create consume claim on %s: %v", coreNode, err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime for %s: %v", coreNode, err)
	}
	return styleID, nodeID
}

// The headline contract, and the one batching could quietly break: a payload's
// lineside is the SUM over every active consume claim that allows it, across
// processes. Batching the per-node reads must not collapse two contributing
// nodes into one.
func TestActivePayloadLineside_SumsAcrossProcesses(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, nodeA := linesideProcess(t, db, "CELL-A", "ALN_001", "aln1", []string{"PART-X"})
	_, nodeB := linesideProcess(t, db, "CELL-B", "ALN_002", "aln2", []string{"PART-X", "PART-Z"})

	if err := db.SetProcessNodeRuntime(nodeA, nil, 25); err != nil {
		t.Fatalf("set runtime A: %v", err)
	}
	if err := db.SetProcessNodeRuntime(nodeB, nil, 7); err != nil {
		t.Fatalf("set runtime B: %v", err)
	}

	got := NewStationService(db).activePayloadLineside(true)

	if got["PART-X"] != 32 { // 25 from CELL-A + 7 from CELL-B
		t.Errorf("PART-X = %d, want 32 (25 from CELL-A + 7 from CELL-B)", got["PART-X"])
	}
	if got["PART-Z"] != 7 { // only CELL-B allows it
		t.Errorf("PART-Z = %d, want 7 (CELL-B only)", got["PART-Z"])
	}
}

// Runtime UOP and active lineside buckets both contribute, summed per node.
func TestActivePayloadLineside_SumsRuntimeAndBuckets(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	styleID, nodeA := linesideProcess(t, db, "CELL-A", "ALN_001", "aln1", []string{"PART-X"})

	if err := db.SetProcessNodeRuntime(nodeA, nil, 25); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeA, "", styleID, "PART-X", 15); err != nil {
		t.Fatalf("capture bucket: %v", err)
	}

	got := NewStationService(db).activePayloadLineside(true)

	if got["PART-X"] != 40 {
		t.Errorf("PART-X = %d, want 40 (25 runtime + 15 bucket)", got["PART-X"])
	}
}

// A claim allowing several payloads attributes its node's FULL total to each,
// rather than dividing it. Preserved from the per-node implementation.
func TestActivePayloadLineside_AttributesFullTotalToEachPayload(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, nodeA := linesideProcess(t, db, "CELL-A", "ALN_001", "aln1", []string{"PART-X", "PART-Y"})
	if err := db.SetProcessNodeRuntime(nodeA, nil, 40); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	got := NewStationService(db).activePayloadLineside(true)

	if got["PART-X"] != 40 || got["PART-Y"] != 40 {
		t.Errorf("PART-X = %d, PART-Y = %d, want 40 each (full total to each, not divided)",
			got["PART-X"], got["PART-Y"])
	}
}

// The gate must skip the plant-wide scan when no tile would read it, and must
// not change the result when it is on.
func TestActivePayloadLineside_GateSkipsScan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, nodeA := linesideProcess(t, db, "CELL-A", "ALN_001", "aln1", []string{"PART-X"})
	if err := db.SetProcessNodeRuntime(nodeA, nil, 99); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	svc := NewStationService(db)
	if got := svc.activePayloadLineside(false); len(got) != 0 {
		t.Errorf("gate off returned %v, want empty", got)
	}
	if got := svc.activePayloadLineside(true); got["PART-X"] != 99 {
		t.Errorf("gate on returned PART-X = %d, want 99 — the gate must not change the result", got["PART-X"])
	}
}
