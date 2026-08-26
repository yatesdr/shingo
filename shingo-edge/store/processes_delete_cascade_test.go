package store

import (
	"errors"
	"testing"

	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// Deleting a process used to be one statement with the children left to
// ON DELETE CASCADE — and edge SQLite runs with foreign keys OFF, so that
// cascade never fired. Every process ever deleted left its styles, nodes and
// stations behind. The live Springfield edge carried five orphaned styles and an
// orphaned process_node that SetNodes was still willing to ADOPT by name onto a
// live station, an hour after the process that made it was deleted.
//
// These pin the two halves that matter: what must be gone, and what must NOT be
// touched on the way past.

// seedProcessWithChildren builds a process carrying one of everything the
// cascade has an opinion about.
func seedProcessWithChildren(t *testing.T, db *DB, name string) (processID, styleID, nodeID, stationID int64) {
	t.Helper()
	processID, err := db.CreateProcess(name, "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	styleID, err = db.CreateStyle("S-"+name, "", processID)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}
	if _, err := db.CreateReportingPoint("PLC-"+name, "TAG-"+name, styleID); err != nil {
		t.Fatalf("CreateReportingPoint: %v", err)
	}
	stationID, err = db.CreateOperatorStation(stations.Input{ProcessID: processID, Name: "ST-" + name})
	if err != nil {
		t.Fatalf("CreateOperatorStation: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "N-" + name, Name: "N-" + name, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("EnsureProcessNodeRuntime: %v", err)
	}
	// The permanent production record, and the two name-keyed mirrors.
	if _, err := db.Exec(`INSERT INTO hourly_counts (process_id, style_id, count_date, hour, delta)
		VALUES (?, ?, '2026-08-26', 9, 42)`, processID, styleID); err != nil {
		t.Fatalf("insert hourly: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO daily_counts (process_id, style_id, count_date, total)
		VALUES (?, ?, '2026-08-26', 42)`, processID, styleID); err != nil {
		t.Fatalf("insert daily: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sourcing_state (process_id, style_id, status)
		VALUES (?, 'Default', 'green')`, name); err != nil {
		t.Fatalf("insert sourcing_state: %v", err)
	}
	return processID, styleID, nodeID, stationID
}

func count(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", q, err)
	}
	return n
}

func TestDeleteProcess_RetiresItsChildren(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, sid, nid, stid := seedProcessWithChildren(t, db, "Bin Unloader")

	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}

	if n := count(t, db, `SELECT COUNT(*) FROM processes WHERE id=?`, pid); n != 0 {
		t.Errorf("process row survived: %d", n)
	}

	// THE ONE THAT CAUSED THE INCIDENT. Soft delete rather than hard, because
	// changeover_node_tasks would CASCADE-destroy its detail — but retired is
	// what matters here: ListNodesByProcess filters on deleted_at, so the row
	// can no longer be adopted by core_node_name.
	if n := count(t, db, `SELECT COUNT(*) FROM process_nodes WHERE id=? AND deleted_at IS NOT NULL`, nid); n != 1 {
		t.Errorf("process_node not retired")
	}
	live, err := processes.ListNodesByProcess(db.DB, pid)
	if err != nil {
		t.Fatalf("ListNodesByProcess: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("retired node still adoptable: %+v", live)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM process_node_runtime_states WHERE process_node_id=?`, nid); n != 0 {
		t.Errorf("runtime state survived: stale state for an unaddressable node")
	}

	// Styles retire and their reporting points stop polling, exactly as
	// DeleteStyle does it.
	if n := count(t, db, `SELECT COUNT(*) FROM styles WHERE id=? AND deleted_at IS NOT NULL`, sid); n != 1 {
		t.Errorf("style not retired")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM reporting_points WHERE style_id=? AND enabled=1`, sid); n != 0 {
		t.Errorf("reporting point still enabled — it would keep polling a PLC for a dead style")
	}

	if n := count(t, db, `SELECT COUNT(*) FROM operator_stations WHERE id=?`, stid); n != 0 {
		t.Errorf("operator_station survived: it lists via LEFT JOIN, so it would render with a blank process")
	}
	// Keyed on the process NAME, so a later process reusing the name would
	// inherit the stale row.
	if n := count(t, db, `SELECT COUNT(*) FROM sourcing_state WHERE process_id='Bin Unloader'`); n != 0 {
		t.Errorf("sourcing_state survived")
	}
}

// The production record is the thing a config action must never be able to
// reach. hourly_counts and daily_counts carry no FK precisely so that a process
// delete cannot take them, and this asserts the cascade honours that rather
// than re-introducing the defect by hand.
func TestDeleteProcess_LeavesTheProductionRecord(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _, _, _ := seedProcessWithChildren(t, db, "SNF9")

	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}

	if n := count(t, db, `SELECT COUNT(*) FROM hourly_counts WHERE process_id=?`, pid); n != 1 {
		t.Errorf("hourly_counts destroyed by a config action: got %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM daily_counts WHERE process_id=?`, pid); n != 1 {
		t.Errorf("daily_counts destroyed by a config action: got %d", n)
	}
}

// A lineside bucket is an inventory record — it says how many parts are at a
// node right now. Refusing is both the safe answer and the useful one.
func TestDeleteProcess_RefusesWhileStockIsBooked(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, sid, nid, _ := seedProcessWithChildren(t, db, "P-Stocked")

	if _, err := db.Exec(`INSERT INTO node_lineside_bucket (node_id, style_id, part_number, qty)
		VALUES (?, ?, '76682-6TA0A.06', 240)`, nid, sid); err != nil {
		t.Fatalf("insert bucket: %v", err)
	}

	err := db.DeleteProcess(pid)
	if !errors.Is(err, processes.ErrProcessHasStock) {
		t.Fatalf("expected ErrProcessHasStock, got %v", err)
	}
	// The refusal must be total: a half-applied cascade would be worse than
	// either outcome.
	if n := count(t, db, `SELECT COUNT(*) FROM processes WHERE id=?`, pid); n != 1 {
		t.Errorf("process deleted despite the refusal")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM styles WHERE id=? AND deleted_at IS NULL`, sid); n != 1 {
		t.Errorf("style retired despite the refusal — the delete was not atomic")
	}

	// An emptied bucket is not stock. Draining it releases the delete.
	if _, err := db.Exec(`UPDATE node_lineside_bucket SET qty=0 WHERE node_id=?`, nid); err != nil {
		t.Fatalf("drain bucket: %v", err)
	}
	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess after draining: %v", err)
	}
}

// Deleting something that is already gone is not an error — the handler maps a
// failure to a 500, and a double-click is not a server fault.
func TestDeleteProcess_MissingIsNotAnError(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	if err := db.DeleteProcess(99999); err != nil {
		t.Fatalf("deleting a missing process: %v", err)
	}
}
