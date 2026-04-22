package lineside

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openTestDB creates a fresh SQLite DB, runs just enough of the edge
// schema to satisfy the FK constraints on node_lineside_bucket, and
// seeds a process, style, and node.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "lineside.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ddl := `
CREATE TABLE processes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT ''
);
CREATE TABLE styles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    UNIQUE(process_id, name)
);
CREATE TABLE process_nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    core_node_name TEXT NOT NULL,
    code TEXT NOT NULL,
    name TEXT NOT NULL
);
CREATE TABLE node_lineside_bucket (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id      INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    pair_key     TEXT NOT NULL DEFAULT '',
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    part_number  TEXT NOT NULL,
    qty          INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_lineside_active_unique
    ON node_lineside_bucket(node_id, style_id, part_number)
    WHERE state = 'active';
CREATE INDEX idx_lineside_node_state ON node_lineside_bucket(node_id, state);
CREATE INDEX idx_lineside_pair_state ON node_lineside_bucket(pair_key, state) WHERE pair_key != '';

INSERT INTO processes (id, name) VALUES (1, 'Line 1');
INSERT INTO styles (id, process_id, name) VALUES (10, 1, 'StyleA'), (20, 1, 'StyleB');
INSERT INTO process_nodes (id, process_id, core_node_name, code, name)
    VALUES (100, 1, 'ALN_002', 'aln-002', 'ALN_002'),
           (101, 1, 'ALN_003', 'aln-003', 'ALN_003');
`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	return db
}

func TestCaptureCreatesActiveBucket(t *testing.T) {
	db := openTestDB(t)

	b, err := Capture(db, 100, "", 10, "P-500", 60)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if b == nil {
		t.Fatal("Capture returned nil bucket")
	}
	if b.State != StateActive || b.Qty != 60 {
		t.Fatalf("bucket state=%s qty=%d, want active/60", b.State, b.Qty)
	}
	if b.StyleID != 10 || b.PartNumber != "P-500" || b.NodeID != 100 {
		t.Fatalf("bucket identifiers wrong: %+v", b)
	}
}

func TestCaptureZeroIsNoop(t *testing.T) {
	db := openTestDB(t)
	b, err := Capture(db, 100, "", 10, "P-500", 0)
	if err != nil {
		t.Fatalf("Capture 0: %v", err)
	}
	if b != nil {
		t.Fatalf("expected nil bucket for zero qty, got %+v", b)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket`).Scan(&count)
	if count != 0 {
		t.Fatalf("expected no rows inserted, got %d", count)
	}
}

func TestCaptureMergesWithExistingActive(t *testing.T) {
	db := openTestDB(t)
	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("first Capture: %v", err)
	}
	if _, err := Capture(db, 100, "", 10, "P-500", 25); err != nil {
		t.Fatalf("merge Capture: %v", err)
	}

	b, err := GetActive(db, 100, 10, "P-500")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if b.Qty != 85 {
		t.Fatalf("merged qty=%d, want 85", b.Qty)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket`).Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 row after merge, got %d", count)
	}
}

func TestCaptureReactivatesInactive(t *testing.T) {
	db := openTestDB(t)

	// Style A runs, captures 60 of P-500.
	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("Capture A: %v", err)
	}
	// Switch to Style B — A's bucket goes inactive.
	if err := DeactivateOtherStyles(db, 100, 20); err != nil {
		t.Fatalf("DeactivateOtherStyles: %v", err)
	}
	// A's bucket is now inactive with qty 60.
	b, err := Find(db, 100, 10, "P-500")
	if err != nil {
		t.Fatalf("Find inactive: %v", err)
	}
	if b.State != StateInactive || b.Qty != 60 {
		t.Fatalf("inactive bucket wrong: state=%s qty=%d", b.State, b.Qty)
	}

	// Switch back to Style A — operator captures another 10 to lineside.
	if _, err := Capture(db, 100, "", 10, "P-500", 10); err != nil {
		t.Fatalf("Capture reactivate: %v", err)
	}
	b2, err := GetActive(db, 100, 10, "P-500")
	if err != nil {
		t.Fatalf("GetActive after reactivate: %v", err)
	}
	if b2.Qty != 70 {
		t.Fatalf("reactivated merged qty=%d, want 70 (60 stranded + 10 fresh)", b2.Qty)
	}

	// Still exactly one row for this (node, style, part).
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket
		WHERE node_id=? AND style_id=? AND part_number=?`,
		100, 10, "P-500").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 row after reactivate, got %d", count)
	}
}

func TestDeactivateOtherStylesLeavesKeptStyleUntouched(t *testing.T) {
	db := openTestDB(t)
	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("Capture A: %v", err)
	}
	if _, err := Capture(db, 100, "", 20, "P-600", 40); err != nil {
		// Both active for different styles is a transient state — the
		// unique index allows it because part_number differs. This is
		// the post-capture / pre-deactivate condition inside a release
		// transaction.
		t.Fatalf("Capture B: %v", err)
	}

	if err := DeactivateOtherStyles(db, 100, 20); err != nil {
		t.Fatalf("DeactivateOtherStyles: %v", err)
	}

	a, err := Find(db, 100, 10, "P-500")
	if err != nil {
		t.Fatalf("Find A: %v", err)
	}
	if a.State != StateInactive {
		t.Fatalf("A bucket should be inactive, got %s", a.State)
	}

	b, err := GetActive(db, 100, 20, "P-600")
	if err != nil {
		t.Fatalf("GetActive B: %v", err)
	}
	if b.Qty != 40 {
		t.Fatalf("B bucket qty=%d, want 40", b.Qty)
	}
}

func TestDrainDecrementsBucketFirst(t *testing.T) {
	db := openTestDB(t)
	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	drained, err := Drain(db, 100, 10, "P-500", 15)
	if err != nil {
		t.Fatalf("Drain 15: %v", err)
	}
	if drained != 15 {
		t.Fatalf("drained=%d, want 15", drained)
	}
	b, _ := GetActive(db, 100, 10, "P-500")
	if b.Qty != 45 {
		t.Fatalf("qty after drain=%d, want 45", b.Qty)
	}
}

func TestDrainCarriesRemainderWhenBucketEmpty(t *testing.T) {
	db := openTestDB(t)
	if _, err := Capture(db, 100, "", 10, "P-500", 10); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	drained, err := Drain(db, 100, 10, "P-500", 25)
	if err != nil {
		t.Fatalf("Drain 25: %v", err)
	}
	if drained != 10 {
		t.Fatalf("drained=%d, want 10 (the bucket qty)", drained)
	}
	// Bucket was deleted because it hit zero.
	_, err = GetActive(db, 100, 10, "P-500")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows after drain-to-zero, got %v", err)
	}
}

func TestDrainWithNoBucketReturnsZero(t *testing.T) {
	db := openTestDB(t)
	drained, err := Drain(db, 100, 10, "P-500", 5)
	if err != nil {
		t.Fatalf("Drain empty: %v", err)
	}
	if drained != 0 {
		t.Fatalf("drained=%d, want 0", drained)
	}
}

func TestDrainZeroIsNoop(t *testing.T) {
	db := openTestDB(t)
	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	drained, err := Drain(db, 100, 10, "P-500", 0)
	if err != nil {
		t.Fatalf("Drain 0: %v", err)
	}
	if drained != 0 {
		t.Fatalf("drained=%d, want 0", drained)
	}
	b, _ := GetActive(db, 100, 10, "P-500")
	if b.Qty != 60 {
		t.Fatalf("qty should be untouched, got %d", b.Qty)
	}
}

func TestListForNodeActiveFirst(t *testing.T) {
	db := openTestDB(t)
	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("Capture A: %v", err)
	}
	// DeactivateOtherStyles: leaves no active rows for style 10 — but we
	// simulate a stranded bucket by flipping state directly then starting
	// a new active style.
	if err := DeactivateOtherStyles(db, 100, 20); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, err := Capture(db, 100, "", 20, "P-600", 40); err != nil {
		t.Fatalf("Capture B: %v", err)
	}

	list, err := ListForNode(db, 100)
	if err != nil {
		t.Fatalf("ListForNode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}
	if list[0].State != StateActive {
		t.Fatalf("first row should be active, got %s", list[0].State)
	}
	if list[1].State != StateInactive {
		t.Fatalf("second row should be inactive, got %s", list[1].State)
	}
}

func TestListForPair(t *testing.T) {
	db := openTestDB(t)
	if _, err := Capture(db, 100, "pair-1", 10, "P-500", 60); err != nil {
		t.Fatalf("Capture node A: %v", err)
	}
	if _, err := Capture(db, 101, "pair-1", 10, "P-500", 20); err != nil {
		t.Fatalf("Capture node B: %v", err)
	}

	list, err := ListForPair(db, "pair-1")
	if err != nil {
		t.Fatalf("ListForPair: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows for pair, got %d", len(list))
	}
}

func TestListForPairEmptyKey(t *testing.T) {
	db := openTestDB(t)
	list, err := ListForPair(db, "")
	if err != nil {
		t.Fatalf("ListForPair empty: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(list))
	}
}

func TestUniqueActivePerNodeStylePart(t *testing.T) {
	db := openTestDB(t)
	// Two direct inserts of "active" for the same (node, style, part)
	// must be rejected by the unique index — Capture always merges so
	// this exercises the index itself.
	if _, err := db.Exec(`INSERT INTO node_lineside_bucket
		(node_id, pair_key, style_id, part_number, qty, state)
		VALUES (100, '', 10, 'P-500', 60, 'active')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := db.Exec(`INSERT INTO node_lineside_bucket
		(node_id, pair_key, style_id, part_number, qty, state)
		VALUES (100, '', 10, 'P-500', 10, 'active')`)
	if err == nil {
		t.Fatal("expected unique-index violation on second active insert, got nil")
	}
}

func TestDeleteRemovesBucket(t *testing.T) {
	db := openTestDB(t)
	b, err := Capture(db, 100, "", 10, "P-500", 60)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := Delete(db, b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket`).Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 rows after delete, got %d", count)
	}
}

func TestDeactivateDeletesZeroQtyRows(t *testing.T) {
	db := openTestDB(t)
	// Manually seed a zero-qty active row (wouldn't normally exist, but
	// we want to confirm the cleanup happens).
	if _, err := db.Exec(`INSERT INTO node_lineside_bucket
		(node_id, pair_key, style_id, part_number, qty, state)
		VALUES (100, '', 10, 'P-500', 0, 'active')`); err != nil {
		t.Fatalf("seed zero row: %v", err)
	}
	if err := DeactivateOtherStyles(db, 100, 20); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket`).Scan(&count)
	if count != 0 {
		t.Fatalf("expected zero-qty row to be deleted, got %d rows", count)
	}
}
