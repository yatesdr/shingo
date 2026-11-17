package store

import (
	"path/filepath"
	"testing"

	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// THE BOOT CLEAN-UP AND A RETIRED POSITION (round 11). Taking a position off
// retires its row (deleted_at); putting the same node back makes a new row.
// The unique index allows that pair (it is live rows only), and the boot's
// collapseDuplicateProcessNodes must leave it alone: a retired row is history,
// not a duplicate. Before round 11 the collapse grouped retired rows too, so
// every remove-and-re-add was "merged" at the next start, the retired row
// hard-deleted with a "LOSES this node" warning, and in one shape the LIVE row
// was the one deleted. Each pin goes through a real restart (Open).

// retiredPlant opens a database file with one process and one screen.
func retiredPlant(t *testing.T) (path string, db *DB, pid, sid int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "edge.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if pid, err = db.CreateProcess("RET-PROC", "", "", "", false); err != nil {
		t.Fatalf("create process: %v", err)
	}
	if sid, err = db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "RET-SCREEN", Enabled: true}); err != nil {
		t.Fatalf("create screen: %v", err)
	}
	return path, db, pid, sid
}

func mustPosition(t *testing.T, db *DB, pid int64, sid *int64, name string) int64 {
	t.Helper()
	id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, OperatorStationID: sid, CoreNodeName: name,
		Name: name, Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create position %s: %v", name, err)
	}
	if _, err := db.EnsureProcessNodeRuntime(id); err != nil {
		t.Fatalf("runtime for %s: %v", name, err)
	}
	return id
}

// restart closes and reopens the database, as an Edge start does.
func restart(t *testing.T, path string, db *DB) *DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// rowsOf is every row (live and retired) for a node, by id.
func rowsOf(t *testing.T, db *DB, name string) map[int64]bool {
	t.Helper()
	rows, err := db.Query(`SELECT id, deleted_at IS NULL FROM process_nodes WHERE core_node_name = ?`, name)
	if err != nil {
		t.Fatalf("rows of %s: %v", name, err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var live bool
		if err := rows.Scan(&id, &live); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = live
	}
	return out
}

// The shape round 10 did not test: a position taken off a screen (its retired
// row keeps the screen), then put back on no screen. The collapse picked the
// survivor "station-bound first", which was the retired row, and deleted the
// live one: the position was gone after the restart.
func TestCollapseRetired_APositionPutBackOnNoScreenSurvivesARestart(t *testing.T) {
	t.Parallel()
	path, db, pid, sid := retiredPlant(t)
	old := mustPosition(t, db, pid, &sid, "RET-W1")
	if err := db.DeleteProcessNode(old); err != nil {
		t.Fatalf("take it off: %v", err)
	}
	back := mustPosition(t, db, pid, nil, "RET-W1")

	db = restart(t, path, db)
	got := rowsOf(t, db, "RET-W1")
	if live, ok := got[back]; !ok || !live {
		t.Errorf("after the restart rows = %v: the live position %d, put back on no screen, is gone", got, back)
	}
	if live, ok := got[old]; !ok || live {
		t.Errorf("after the restart rows = %v: the retired row %d should stay retired, as it was", got, old)
	}
}

// The shape the Edge's take and a person's screen Save leave: off a screen and
// back on the same screen. The live row survived, but the retired row was
// hard-deleted with "station N LOSES this node from its board".
func TestCollapseRetired_APositionPutBackOnItsScreenChangesNothingAtARestart(t *testing.T) {
	t.Parallel()
	path, db, pid, sid := retiredPlant(t)
	old := mustPosition(t, db, pid, &sid, "RET-W2")
	if err := db.DeleteProcessNode(old); err != nil {
		t.Fatalf("take it off: %v", err)
	}
	back := mustPosition(t, db, pid, &sid, "RET-W2")

	db = restart(t, path, db)
	got := rowsOf(t, db, "RET-W2")
	if len(got) != 2 || !got[back] || got[old] {
		t.Errorf("after the restart rows = %v, want the live %d and the retired %d both as they were", got, back, old)
	}
}

// Live duplicates are still collapsed: the clean-up's own job, unchanged.
func TestCollapseRetired_LiveDuplicatesAreStillCollapsed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, live, orphanWithBin, orphanBinless := seedDupProcessNodes(t, db)
	// A retired row of the same node beside them is left alone.
	if _, err := db.Exec(`INSERT INTO process_nodes (id, process_id, core_node_name, code, name, deleted_at)
		VALUES (40, 1, 'PLN_01', 'pln-01-old', 'PLN_01', datetime('now'))`); err != nil {
		t.Fatalf("seed retired: %v", err)
	}
	if err := db.collapseDuplicateProcessNodes(); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	got := rowsOf(t, db, "PLN_01")
	if !got[live] || len(got) != 2 {
		t.Errorf("rows = %v, want the live %d and the retired 40 only (orphans %d, %d collapsed)", got, live, orphanWithBin, orphanBinless)
	}
	if live40, ok := got[40]; !ok || live40 {
		t.Errorf("the retired row 40 was touched: rows = %v", got)
	}
}
