//go:build docker

package testdb

import (
	"testing"
	"time"

	"shingocore/store"
)

// TestTemplateDB_HasAllSchema validates that the template database actually
// has the schema we expect. If a future migration is forgotten in the
// template build, this test fails fast and loud instead of letting opaque
// "table does not exist" errors propagate across the rest of the suite.
//
// The expected version is derived from the migration list itself
// (store.LatestMigrationVersion), so it stays exact without per-migration
// maintenance: the applied max in the template must equal the highest
// migration the build defines. A mismatch means the template skipped a
// migration (a stale template build).
func TestTemplateDB_HasAllSchema(t *testing.T) {
	db := Open(t)

	var maxVersion int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&maxVersion); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if want := store.LatestMigrationVersion(); maxVersion != want {
		t.Errorf("template schema_migrations max version = %d, want %d (template build skipped a migration)", maxVersion, want)
	}

	// Core tables every test depends on. Not exhaustive — failure here
	// means the migration that creates this table never ran against the
	// template.
	coreTables := []string{
		"schema_migrations",
		"orders",
		"bins",
		"nodes",
		"payloads",
		"order_bins",
		"bin_uop_audit",
		"lineside_buckets",
		"inventory_delta_dedup",
	}
	for _, tbl := range coreTables {
		var exists bool
		err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`, tbl).Scan(&exists)
		if err != nil {
			t.Fatalf("introspect %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("template missing table %q", tbl)
		}
	}
}

// TestTemplateDB_CloneIsFast asserts that cloning a fresh test DB from the
// template stays under 500ms. A regression past this bound indicates either
// schema bloat in the template or a Postgres lock-serialization problem under
// concurrency.
//
// The first Open(t) in this test pays template setup cost (migrations on
// the template DB) and so is excluded from the measurement. The second
// Open(t) is the actual benchmark.
func TestTemplateDB_CloneIsFast(t *testing.T) {
	// Warm the template — first Open pays setup cost.
	_ = Open(t)

	start := time.Now()
	_ = Open(t)
	elapsed := time.Since(start)

	const threshold = 500 * time.Millisecond
	if elapsed > threshold {
		t.Errorf("template clone took %v, trigger threshold is %v — investigate before assuming the speedup holds", elapsed, threshold)
	} else {
		t.Logf("template clone wall-clock: %v (threshold %v)", elapsed, threshold)
	}
}

// TestTemplateDB_TerminateBackendRate fails if pg_terminate_backend cleanup
// fires on more than 5% of test teardowns. Connection leaks somewhere in
// production code show up here — DROP DATABASE blocks because something
// didn't release its pool before the test ended, and we have to nuke the
// session to make cleanup succeed.
//
// Runs at the end of the test order, after all other tests in the package
// have populated the counters. Go's per-file alphabetical Test ordering is
// not contractual, but within a package _smoke_test.go sorts last, so this
// is a best-effort post-suite assertion.
func TestTemplateDB_TerminateBackendRate(t *testing.T) {
	// Ensure at least one DB was created so the ratio is meaningful.
	_ = Open(t)

	created := TestDatabasesCreated()
	fired := TerminateBackendFired()
	if created == 0 {
		t.Skip("no test databases created — counters empty")
	}
	ratio := float64(fired) / float64(created)
	const threshold = 0.05
	if ratio > threshold {
		t.Errorf("pg_terminate_backend fired on %d / %d cleanups (%.1f%%), trigger threshold is %.1f%% — likely a connection leak in production code",
			fired, created, ratio*100, threshold*100)
	} else {
		t.Logf("pg_terminate_backend rate: %d / %d (%.2f%%, threshold %.1f%%)", fired, created, ratio*100, threshold*100)
	}
}
