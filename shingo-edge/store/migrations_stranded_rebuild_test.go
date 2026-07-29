package store_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"shingoedge/store"
)

// The v33 rebuild is four statements and modernc.org/sqlite does NOT run them
// atomically, so process death between the RENAME and the DROP leaves
// style_node_claims_legacy holding the plant's claims.
//
// Nothing else in the startup path detects that state, which is why these
// tests exist. rebuildStyleNodeClaims decides whether to run by reading the
// LIVE table's column defaults, and a half-finished rebuild has already written
// the new correct defaults — so the guard says "nothing to do". verifySchema
// passes for the same reason: every required table and column is present, and
// only the CONTENTS are wrong.
//
// Measured against the 2026-07-27 Springfield dump: killing the real rebuild
// after statement 1 or 2 left 0 live claims and 35 stranded, and store.Open()
// returned no error.

// strandedAfter builds a database in the state an interrupted rebuild leaves:
// a live style_node_claims carrying the NEW shape with liveRows rows, beside a
// style_node_claims_legacy carrying legacyRows.
//
// The live table is given the post-rebuild defaults deliberately (auto_reorder
// DEFAULT 0, swap_mode with none). That is what makes this a regression test
// rather than a tautology — it is the exact shape that convinces the existing
// idempotency guard the work is already done.
func strandedAfter(t *testing.T, liveRows, legacyRows int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stranded.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	if _, err := raw.Exec(`
		CREATE TABLE styles (
		    id INTEGER PRIMARY KEY AUTOINCREMENT,
		    name TEXT NOT NULL
		);
		CREATE TABLE style_node_claims (
		    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		    style_id             INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
		    core_node_name       TEXT NOT NULL,
		    role                 TEXT NOT NULL DEFAULT 'consume',
		    swap_mode            TEXT NOT NULL,
		    auto_reorder         INTEGER NOT NULL DEFAULT 0,
		    reorder_point_source TEXT NOT NULL DEFAULT 'legacy',
		    created_at           TEXT NOT NULL DEFAULT (datetime('now')),
		    UNIQUE(style_id, core_node_name)
		);
		CREATE TABLE style_node_claims_legacy (
		    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		    style_id             INTEGER NOT NULL,
		    core_node_name       TEXT NOT NULL,
		    role                 TEXT NOT NULL DEFAULT 'consume',
		    swap_mode            TEXT NOT NULL DEFAULT 'simple',
		    auto_reorder         INTEGER NOT NULL DEFAULT 1,
		    reorder_point_source TEXT NOT NULL DEFAULT 'legacy',
		    created_at           TEXT NOT NULL DEFAULT (datetime('now'))
		);
		INSERT INTO styles (id, name) VALUES (1, 'RUN-A');`); err != nil {
		t.Fatalf("build stranded shape: %v", err)
	}

	for i := 1; i <= legacyRows; i++ {
		if _, err := raw.Exec(
			`INSERT INTO style_node_claims_legacy (id, style_id, core_node_name, swap_mode)
			 VALUES (?, 1, ?, 'simple')`, i, "NODE_"+string(rune('A'+i-1))); err != nil {
			t.Fatalf("seed legacy row %d: %v", i, err)
		}
	}
	for i := 1; i <= liveRows; i++ {
		if _, err := raw.Exec(
			`INSERT INTO style_node_claims (id, style_id, core_node_name, swap_mode)
			 VALUES (?, 1, ?, 'simple')`, i, "NODE_"+string(rune('A'+i-1))); err != nil {
			t.Fatalf("seed live row %d: %v", i, err)
		}
	}
	return path
}

// The data-loss case: the copy never ran, so the live table is short. Edge must
// refuse to start rather than run the plant on an incomplete claim set.
func TestStrandedRebuild_IncompleteCopyRefusesToStart(t *testing.T) {
	path := strandedAfter(t, 0, 3)

	db, err := store.Open(path)
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded on a database whose claims are stranded in " +
			"style_node_claims_legacy — this is the silent failure the guard exists to stop")
	}

	// The message has to name both counts and the recovery, because whoever
	// reads it is standing at a plant deciding whether to roll back.
	for _, want := range []string{"style_node_claims_legacy", "stranded", "0 row", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("startup error does not mention %q, so it does not tell the operator what happened:\n%v", want, err)
		}
	}
}

// The survivable case: the copy completed and only the DROP was lost. Finishing
// it is what the interrupted run intended. Refusing to start here would block a
// plant whose data is entirely correct.
func TestStrandedRebuild_CompletedCopyFinishesTheDrop(t *testing.T) {
	path := strandedAfter(t, 3, 3)

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open refused a database whose copy had completed — only the DROP was lost: %v", err)
	}
	defer db.Close()

	var leftovers int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='style_node_claims_legacy'`).
		Scan(&leftovers); err != nil {
		t.Fatalf("check leftovers: %v", err)
	}
	if leftovers != 0 {
		t.Error("the interrupted DROP was not completed, so every later start re-enters this branch " +
			"and the scratch table's dangling REFERENCES clause never goes away")
	}

	var claims int
	if err := db.QueryRow(`SELECT count(*) FROM style_node_claims`).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims != 3 {
		t.Errorf("claims = %d, want 3 — completing the DROP must not touch the live rows", claims)
	}
}

// A database that was never migrated must still migrate normally. This is the
// case the guard must NOT catch: it has the old defaults and no scratch table,
// and it is what every plant edge looks like today.
func TestStrandedRebuild_LeavesANormalMigrationAlone(t *testing.T) {
	db, _ := openAgedEdge(t)

	var claims int
	if err := db.QueryRow(`SELECT count(*) FROM style_node_claims`).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims != 2 {
		t.Errorf("claims = %d, want 2 — the stranded-rebuild guard must not interfere with a normal migration", claims)
	}
}
