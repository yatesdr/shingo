package stations

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// station_node_names_test.go — GetNodeNames answers a screen's LIVE positions.
//
// The list behind the desktop's screen sheet and its position picker is a
// wholesale-replace editor: whatever GetNodeNames returns is what the engineer
// sees claimed, and the save sends exactly that list back. A retired row still
// answering means a position the operator removed on this very screen comes
// back on the next save — the removal cannot hold. Unlike the by-id and
// by-name resolution paths, which resolve an identifier somebody already holds
// and therefore see tombstones, this read feeds a picker, and pickers show
// what exists.
//
// The stations package had no test file of its own before this one; the
// helpers below create the three tables the query touches, in the canonical
// column shapes (applying the full schema DDL here would import a cycle), so
// the query under test runs against the shape it runs against in production.

func openStationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "stations.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE processes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE TABLE operator_stations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			process_id INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
			code TEXT NOT NULL,
			name TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			area_label TEXT NOT NULL DEFAULT '',
			sequence INTEGER NOT NULL DEFAULT 0,
			controller_node_id TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			health_status TEXT NOT NULL DEFAULT 'offline',
			last_seen_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(process_id, code))`,
		`CREATE TABLE process_nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			process_id INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
			operator_station_id INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
			core_node_name TEXT NOT NULL DEFAULT '',
			code TEXT NOT NULL,
			name TEXT NOT NULL,
			sequence INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			deleted_at TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("seed schema: %v", err)
		}
	}
	return db
}

func mustProcess(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO processes (name) VALUES ('P1')`)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("process id: %v", err)
	}
	return id
}

func mustStation(t *testing.T, db *sql.DB, pid int64, name string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO operator_stations (process_id, code, name, sequence, enabled)
		VALUES (?, 'st-one', ?, 1, 1)`, pid, name)
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("station id: %v", err)
	}
	return id
}

func seedNode(t *testing.T, db *sql.DB, pid, sid int64, coreNode string, seq int) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO process_nodes
		(process_id, operator_station_id, core_node_name, code, name, sequence, enabled)
		VALUES (?, ?, ?, ?, ?, ?, 1)`, pid, sid, coreNode, coreNode, coreNode, seq)
	if err != nil {
		t.Fatalf("create node %s: %v", coreNode, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("node id: %v", err)
	}
	return id
}

// retireNode is the store's own retire shape (processes.DeleteNode): a stamp,
// not a hard delete, and the station binding stays — only the process-delete
// cascade clears operator_station_id, and a single removed position never
// goes through it.
func retireNode(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE process_nodes
		SET deleted_at = datetime('now'), updated_at = datetime('now')
		WHERE id = ? AND deleted_at IS NULL`, id); err != nil {
		t.Fatalf("retire node %d: %v", id, err)
	}
}

func TestGetNodeNames_HidesRetiredRows(t *testing.T) {
	db := openStationDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	}()

	pid := mustProcess(t, db)
	sid := mustStation(t, db, pid, "Screen 1")

	seedNode(t, db, pid, sid, "PLN_001", 1)
	retired := seedNode(t, db, pid, sid, "PLN_002", 2)
	seedNode(t, db, pid, sid, "PLN_003", 3)

	retireNode(t, db, retired)

	names, err := GetNodeNames(db, sid)
	if err != nil {
		t.Fatalf("GetNodeNames: %v", err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if got["PLN_002"] {
		t.Fatalf("GetNodeNames returned the retired position PLN_002: %v — this list is what the "+
			"screen sheet shows claimed and what the save sends back, so a removal could not hold",
			names)
	}
	if !got["PLN_001"] || !got["PLN_003"] {
		t.Errorf("GetNodeNames = %v, want the two live positions", names)
	}
}

// The exact row a removal leaves behind: bound to the station, then retired
// with the binding intact. The retired row must not answer for the
// screen.
func TestGetNodeNames_RetiredRowIsNotTheStationAnswer(t *testing.T) {
	db := openStationDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	}()

	pid := mustProcess(t, db)
	sid := mustStation(t, db, pid, "Screen 1")
	n := seedNode(t, db, pid, sid, "PLN_009", 1)
	retireNode(t, db, n)

	names, err := GetNodeNames(db, sid)
	if err != nil {
		t.Fatalf("GetNodeNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("GetNodeNames = %v after retiring its only position, want none — the retired row "+
			"answered for a position the screen no longer has", names)
	}
}
