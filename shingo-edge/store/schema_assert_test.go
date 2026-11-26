package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifySchema_FreshDBSatisfiesManifest keeps the check honest: a database
// born by THIS build satisfies what this build asserts (the baseline's tables
// and columns), or every edge would refuse to start.
func TestVerifySchema_FreshDBSatisfiesManifest(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(dbPath) // Open migrates, then runs verifySchema
	if err != nil {
		t.Fatalf("fresh DB must migrate and verify cleanly, got: %v", err)
	}
	defer db.Close()

	if err := db.verifySchema(); err != nil {
		t.Fatalf("verifySchema on a freshly migrated DB: %v", err)
	}
}

// TestVerifySchema_ReportsMissingObjects proves the assertion actually fires and
// that its message names what is missing — the property that matters during a
// deploy. A dropped column stands in for a partial restore or a hand change.
func TestVerifySchema_ReportsMissingObjects(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "degraded.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Drop one asserted column and one asserted table.
	if _, err := db.Exec("ALTER TABLE orders DROP COLUMN queue_code"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if _, err := db.Exec("DROP TABLE sourcing_state"); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	err = db.verifySchema()
	if err == nil {
		t.Fatal("verifySchema must fail when required objects are missing")
	}

	msg := err.Error()
	for _, want := range []string{"orders.queue_code", "sourcing_state"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message must name the missing object %q; got:\n%s", want, msg)
		}
	}

	// Both gaps reported in one pass, not just the first — an operator needs the
	// whole picture to tell a stale binary from one failed migration.
	if !strings.Contains(msg, "2 required object(s) missing") {
		t.Errorf("expected both misses reported together; got:\n%s", msg)
	}
}

// TestOpen_FailsWhenSchemaIncomplete pins the wiring: a database that does not
// satisfy the manifest must fail Open, not be handed to the engine. This is what
// turns a silent stale-binary deploy into a loud startup failure.
func TestOpen_FailsWhenSchemaIncomplete(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "reopen.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("DROP TABLE sourcing_state"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	db.Close()

	// Re-open. Nothing creates a table again at a start, so this must fail,
	// naming it.
	db2, err := Open(dbPath)
	if err == nil {
		db2.Close()
		t.Fatal("re-open succeeded on a file without sourcing_state")
	}
	if !strings.Contains(err.Error(), "table sourcing_state") {
		t.Errorf("the failure does not name the table: %v", err)
	}
}

// TestSchemaDrift_IsReportedAndRefusesNothing: a difference that is not a
// missing table or column starts the Edge and is one line naming the object;
// a born database reports none.
func TestSchemaDrift_IsReportedAndRefusesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if drift := db.DriftAtBoot(); len(drift) != 0 {
		t.Fatalf("a born database reports drift: %v", drift)
	}
	for _, stmt := range []string{
		"DROP INDEX idx_orders_status",
		"ALTER TABLE orders ADD COLUMN by_hand TEXT",
		"CREATE TABLE left_over (id INTEGER)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("drift refused the start: %v", err)
	}
	defer db2.Close()
	want := []string{
		"schema drift: column orders.by_hand is in the database and not in this build's schema",
		"schema drift: index idx_orders_status is missing (this build ships it)",
		"schema drift: table left_over is in the database and not in this build's schema",
	}
	if got := strings.Join(db2.DriftAtBoot(), "\n"); got != strings.Join(want, "\n") {
		t.Errorf("drift:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}
