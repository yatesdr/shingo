package store

import (
	"database/sql"
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

	// foreign_keys is asserted OFF, BY VALUE, and this is the switch.
	//
	// Enforcement is what makes ON DELETE CASCADE and ON DELETE SET NULL mean
	// anything. With it off — which is every edge that has ever run — deleting a
	// style silently strands its claims: measured on the Springfield dump of
	// 2026-07-27, DELETE FROM styles WHERE id=24 succeeds and takes the
	// orphan-claim count from 8 to 11. That is the defect. This assertion is not
	// defending the defect; it is holding the switch until the thing the switch
	// would break is fixed.
	//
	// WHAT NOW GATES THE FLIP, and what no longer does:
	//
	//   CLOSED — the counter_snapshots restrict. It was NO ACTION on a NOT NULL
	//   column, so the styles -> reporting_points cascade hit it and aborted the
	//   whole delete: 6 of 8 style deletions REFUSED. It is ON DELETE CASCADE
	//   now, and the same probe on the repaired dump refuses 0 of 57.
	//
	//   CLOSED — the source of new dangling clauses. db.rebuildTable holds
	//   PRAGMA legacy_alter_table across every rename-rebuild, so a migration
	//   can no longer re-point a child at a scratch table and drop it.
	//
	//   OPEN, and not closeable from here — the 11 dangling REFERENCES clauses
	//   already written into the Springfield edge's stored schema, and the 1,764
	//   rows whose parent is genuinely gone. Those live on a plant's disk. The
	//   procedure is RUNBOOK-0.5-edge-fk-repair-2026-07-27.md; it is rehearsed
	//   against a restored copy of that database and has not been run on the box.
	//
	// So the remaining condition is a property of A PLANT'S FILE, not of this
	// repository, which is exactly why it cannot be discharged by a commit and
	// why the want below stays 0 until somebody has been to the plant.
	//
	// Measured on the three states of that database, with a probe that rewrites
	// every FK-participating column:
	//
	//	unrepaired                      19 of 37 FK-column writes blocked
	//	after the §0.5 clause repair    12 of 37
	//	after the ordered data plan       1 of 37
	//
	// The point of asserting the VALUE rather than the DSN text is that the flip
	// is a one-word edit — `_pragma=foreign_keys(1)` — whose effect is invisible
	// in review and which would surface at a plant as writes failing rather than
	// as a test failing. Changing the want below is the decision, and the
	// message travels with it so whoever changes it has to answer the question.
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 0 {
		t.Errorf("foreign_keys = %d, want 0 until RUNBOOK-0.5-edge-fk-repair has been RUN at "+
			"each plant and its dangling-clause count measured at 0. The code-side gates "+
			"(counter_snapshots CASCADE, the rename-rebuild guard) are closed; what is left is "+
			"11 dangling REFERENCES clauses and 1,764 orphan rows sitting on Springfield's disk, "+
			"which no commit can fix. Enabling this against an unrepaired edge blocks 19 of 37 "+
			"FK-column writes.", foreignKeys)
	}
}

// The flip must be STAGED, not guessed at: when somebody does change the want
// above, the DSN edit that goes with it has to be the one that works.
//
// This is the Edge DSN-pragma defect class, pinned. The driver is
// modernc.org/sqlite, which reads only `_pragma`; the mattn/go-sqlite3
// spelling that this file's DSN carried for the product's whole life is
// accepted and ignored, so a reviewer reading `_foreign_keys=on` sees
// enforcement that is not there. Asserting both spellings by their resulting
// VALUE is what makes the difference visible before it is deployed rather than
// after.
func TestOpen_ForeignKeyEnforcementIsOneDSNWordAway(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge.db")

	for _, tc := range []struct {
		name string
		dsn  string
		want int
	}{
		{"modernc _pragma spelling — the one that works",
			"file:" + path + "?_pragma=foreign_keys(1)", 1},
		{"mattn _foreign_keys spelling — accepted and IGNORED",
			"file:" + path + "?_foreign_keys=on", 0},
		{"nothing set — today, and SQLite's default",
			"file:" + path, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := sql.Open("sqlite", tc.dsn)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer raw.Close()
			var got int
			if err := raw.QueryRow("PRAGMA foreign_keys").Scan(&got); err != nil {
				t.Fatalf("read foreign_keys: %v", err)
			}
			if got != tc.want {
				t.Errorf("foreign_keys = %d, want %d for DSN %q", got, tc.want, tc.dsn)
			}
		})
	}
}

// A post-open Exec is NOT a substitute for the DSN, and the failure is silent.
//
// SQLite documents that PRAGMA foreign_keys is a no-op inside a transaction,
// and the pragma is per-CONNECTION, so on a pooled *sql.DB it applies to
// whichever connection happened to serve the Exec. store.Open pins
// MaxOpenConns(1), which hides the second problem today and would stop hiding
// it the moment that line changes — so pin the mechanism, not the workaround.
func TestOpen_ForeignKeyPragmaInsideTransactionIsANoOp(t *testing.T) {
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer raw.Close()

	tx, err := raw.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("exec pragma in tx: %v", err)
	}
	var inTx int
	if err := tx.QueryRow("PRAGMA foreign_keys").Scan(&inTx); err != nil {
		t.Fatalf("read in tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if inTx != 0 {
		t.Errorf("PRAGMA foreign_keys = ON inside a transaction took (reads %d) — "+
			"if the driver's behaviour has changed, the argument in store.Open for "+
			"setting this on the DSN needs rechecking, not this test relaxing", inTx)
	}
}
