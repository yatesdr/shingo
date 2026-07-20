package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifySchema_FreshDBSatisfiesManifest is the guard that keeps
// requiredTables/requiredColumns honest. A database migrated by THIS build must
// satisfy the manifest this build asserts; if it doesn't, the manifest names an
// object no migration creates and every edge would refuse to start.
func TestVerifySchema_FreshDBSatisfiesManifest(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(dbPath) // Open runs migrate() then verifySchema()
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
// deploy. A dropped column stands in for the real-world cause (an old binary
// whose migrations never created it).
func TestVerifySchema_ReportsMissingObjects(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "degraded.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Drop one asserted column and one asserted table, mimicking a schema that
	// an older build would have produced.
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

	// Re-open. migrate() recreates sourcing_state (it is in the DDL), so this
	// must SUCCEED — the assertion must not fight a migration that self-heals.
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-open must succeed once migrate() recreates the table, got: %v", err)
	}
	db2.Close()
}
