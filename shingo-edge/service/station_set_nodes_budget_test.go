package service

import (
	"fmt"
	"path/filepath"
	"testing"

	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// station_set_nodes_budget_test.go — what one SetNodes save costs in
// STATEMENTS, by how many names it adds.
//
// The edge store is pinned to a single SQLite connection, so every statement a
// save issues is one the operator board's next poll waits behind. The
// cross-process position check reads every live process node; done once per
// NEW name it made a save's cost grow by a whole-table read per name added.
// It is read once per save now, and only when the save adds a name at all.
//
// The numbers are the measurement, not an ambition. A change that moves one
// must move it in the same commit, with the formula beside it.
//
// Counted through store.OpenCounting, the module's one counting seam.

// countingSetNodesFixture is crossProcessFixture on a counting store: two
// processes, a live position FGN_001 on the second, one station on the first.
func countingSetNodesFixture(t *testing.T) (*StationService, int64, *store.QueryCounter) {
	t.Helper()
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "setnodes-count.db"))
	if err != nil {
		t.Fatalf("open counting store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	pidA, err := db.CreateProcess("Press A", "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess A: %v", err)
	}
	pidB, err := db.CreateProcess("Press B", "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess B: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pidB, CoreNodeName: "FGN_001", Code: "B-POS", Name: "FGN_001",
		Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProcessNode on B: %v", err)
	}
	id, err := db.CreateOperatorStation(stations.Input{ProcessID: pidA, Name: "Loader 1"})
	if err != nil {
		t.Fatalf("CreateOperatorStation: %v", err)
	}
	return NewStationService(db), id, counter
}

// numberedNodeNames returns n distinct names, PREFIX_001 onward.
func numberedNodeNames(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("%s_%03d", prefix, i))
	}
	return out
}

// setNodesBudget is one save size and the statements it costs.
type setNodesBudget struct {
	names, total, reads int64
}

// TestSetNodes_StatementsPerSaveAddingNames pins a save that adds N names no
// process holds.
//
// total = 4 + 5N, reads = 4 + 3N. The flat 4 reads include the one read of
// every live process node for the cross-process check. Read once per new name,
// as it first was, this was total = 3 + 6N, reads = 3 + 4N: the slope moving
// from 4 reads per name down to 3 is that read leaving the loop.
func TestSetNodes_StatementsPerSaveAddingNames(t *testing.T) {
	t.Parallel()
	for _, size := range []setNodesBudget{
		{names: 1, total: 9, reads: 7},
		{names: 3, total: 19, reads: 13},
		{names: 5, total: 29, reads: 19},
	} {
		svc, id, counter := countingSetNodesFixture(t)
		counter.Reset()
		if err := setNodesErr(svc, id, numberedNodeNames("NEW", int(size.names))); err != nil {
			t.Fatalf("%d new names: SetNodes: %v", size.names, err)
		}
		if got := counter.Count(); got != size.total {
			t.Errorf("%d new names: %d statements, pinned at %d (4 + 5N)", size.names, got, size.total)
		}
		if got := counter.Reads(); got != size.reads {
			t.Errorf("%d new names: %d reads, pinned at %d (4 + 3N)", size.names, got, size.reads)
		}
	}
}

// TestSetNodes_StatementsPerResaveAddingNoNames pins a re-save of the
// station's own list: no new name, so the cross-process check reads nothing.
//
// total = 1 + 4N, reads = 2 + 2N — the same as before the check existed.
func TestSetNodes_StatementsPerResaveAddingNoNames(t *testing.T) {
	t.Parallel()
	for _, size := range []setNodesBudget{
		{names: 1, total: 5, reads: 4},
		{names: 3, total: 9, reads: 6},
		{names: 5, total: 13, reads: 8},
	} {
		svc, id, counter := countingSetNodesFixture(t)
		list := numberedNodeNames("OWN", int(size.names))
		if err := setNodesErr(svc, id, list); err != nil {
			t.Fatalf("seed %d names: %v", size.names, err)
		}
		counter.Reset()
		if err := setNodesErr(svc, id, list); err != nil {
			t.Fatalf("re-save of %d: SetNodes: %v", size.names, err)
		}
		if got := counter.Count(); got != size.total {
			t.Errorf("re-save of %d: %d statements, pinned at %d (1 + 4N)", size.names, got, size.total)
		}
		if got := counter.Reads(); got != size.reads {
			t.Errorf("re-save of %d: %d reads, pinned at %d (2 + 2N)", size.names, got, size.reads)
		}
	}
}
