package engine

import (
	"testing"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedWindowNodes creates a process_node per window name, so each window is
// this Edge's (GetProcessNodeByCoreNodeName resolves it: ownWindow, the asks'
// trackers).
func seedWindowNodes(t *testing.T, db *store.DB, proc string, windows []string) {
	t.Helper()
	procID, err := db.CreateProcess(proc, "", "", "", false)
	if err != nil {
		t.Fatalf("create process %s: %v", proc, err)
	}
	for i, w := range windows {
		if _, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, CoreNodeName: w, Code: w, Name: w, Sequence: i + 1, Enabled: true,
		}); err != nil {
			t.Fatalf("create window node %s: %v", w, err)
		}
	}
}
