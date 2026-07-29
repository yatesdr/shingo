package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// Open's connection pragmas must actually be applied.
//
// This is a regression test for a silent failure, not a tautology: the DSN
// carried mattn/go-sqlite3's `_journal_mode=WAL&_busy_timeout=5000` spelling
// while the driver is modernc.org/sqlite, which reads only `_pragma`. Both
// SQLite and the driver ignore unrecognised URI parameters without error, so
// the code read as if WAL were on for the product's whole life while every
// edge ran in rollback-journal mode with no busy timeout. Asserting the
// resulting pragma VALUES (rather than the DSN string) is what catches that
// class of mistake, including a future driver swap that changes the syntax
// again.
func TestOpen_AppliesConnectionPragmas(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal (DSN pragma syntax is driver-specific and fails silently when wrong)", journalMode)
	}

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}

	// foreign_keys is asserted OFF, and that is not this test pinning an
	// accident. Enforcement cannot be switched on until the plant databases are
	// repaired: measured against the Springfield dump of 2026-07-27, enabling it
	// today refuses 11 of 37 FK-touching writes, because 11 REFERENCES clauses
	// still name the pre-rename tables. See the ordered argument in Open's
	// comment.
	//
	// The point of asserting the value is that the change is a one-word DSN edit
	// — `_pragma=foreign_keys(1)` — whose effect is invisible in review and
	// which would surface at a plant as writes failing, not as a test failing.
	// When the repair is done, this assertion is where the flip gets made
	// deliberately: change the want, and the migration ordering has to have
	// happened first.
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 0 {
		t.Errorf("foreign_keys = %d, want 0 until the plant FK repair has shipped "+
			"(enabling it against an unrepaired plant DB blocks writes on 8 tables)", foreignKeys)
	}
}
