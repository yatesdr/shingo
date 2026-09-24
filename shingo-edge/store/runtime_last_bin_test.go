package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// runtime_last_bin_test.go — every statement that can empty a slot remembers
// the bin it held and that bin's stamp, in the same statement.

// TestEmptyingASlotRemembersTheBinThatLeft is the store half of the empty-slot
// guard. Each writer that can set active_bin_id to NULL records the departing
// bin and its active_bin_epoch in last_bin_id / last_bin_epoch, costs exactly
// the one statement it cost before, and a second emptying of an already empty
// slot keeps the record.
func TestEmptyingASlotRemembersTheBinThatLeft(t *testing.T) {
	t.Parallel()
	writers := map[string]func(db *DB, node int64) error{
		"ClearProcessNodeActiveBinAndCount": func(db *DB, node int64) error {
			return db.ClearProcessNodeActiveBinAndCount(node)
		},
		"SetProcessNodeActiveBinID(nil)": func(db *DB, node int64) error {
			return db.SetProcessNodeActiveBinID(node, nil)
		},
		"SetProcessNodeRuntimeWithBin(nil)": func(db *DB, node int64) error {
			return db.SetProcessNodeRuntimeWithBin(node, nil, nil, 0)
		},
		"SetProcessNodeRuntimeWithBinAndEpoch(nil)": func(db *DB, node int64) error {
			return db.SetProcessNodeRuntimeWithBinAndEpoch(node, nil, nil, 0, 0)
		},
		"SetProcessNodeActiveBinIDAndEpoch(nil)": func(db *DB, node int64) error {
			return db.SetProcessNodeActiveBinIDAndEpoch(node, nil, 0)
		},
	}
	for name, empty := range writers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, counter, err := OpenCounting(filepath.Join(t.TempDir(), "last-bin.db"))
			testutil.MustNoErr(t, err, "open counting db")
			t.Cleanup(func() { db.Close() })
			node := seedRuntimeNode(t, db)
			bin := int64(9301)
			testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(node, nil, &bin, 6, 20), "bind")

			counter.Reset()
			testutil.MustNoErr(t, empty(db, node), "empty the slot")
			if got := counter.Count(); got != 1 {
				t.Errorf("emptying issued %d statements, want 1", got)
			}
			testutil.MustNoErr(t, empty(db, node), "empty it again")

			var lastBin *int64
			var lastEpoch int64
			testutil.MustNoErr(t, db.QueryRow(
				`SELECT last_bin_id, last_bin_epoch FROM process_node_runtime_states WHERE process_node_id = ?`,
				node).Scan(&lastBin, &lastEpoch), "read last bin")
			if lastBin == nil || *lastBin != bin || lastEpoch != 6 {
				t.Errorf("last bin = %v at %d, want %d at 6", lastBin, lastEpoch, bin)
			}
		})
	}
}

// seedRuntimeNode creates a process with one node and its runtime row.
func seedRuntimeNode(t *testing.T, db *DB) int64 {
	t.Helper()
	processID, err := db.CreateProcess("LAST-BIN-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "LAST-BIN-NODE", Code: "C-LAST-BIN",
		Name: "LAST-BIN-NODE", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	return nodeID
}
