package engine

import (
	"testing"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedReconcilerNode builds a minimal process+node+style+claim+runtime
// graph for tests that need a fully-set-up consume node. Originally
// lived in the now-deleted uop_reconciler_test.go; preserved here so
// the regression and counter-delta tests that depend on it keep
// compiling.
func seedReconcilerNode(t *testing.T, db *store.DB, prefix, payloadCode string) (nodeID, styleID, claimID int64) {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", prefix+" rec", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: prefix + "-NODE",
		Code:         prefix[:3],
		Name:         prefix + " Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err = db.CreateStyle(prefix+"-STYLE", prefix+" style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)
	claimID, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: prefix + "-NODE",
		Role:         "consume",
		SwapMode:     "simple",
		PayloadCode:  payloadCode,
		UOPCapacity:  100,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	return nodeID, styleID, claimID
}
