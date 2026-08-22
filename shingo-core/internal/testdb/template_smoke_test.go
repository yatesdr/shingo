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
//
// THE EXPECTATION HAS TO BE FORCED INTO EXISTENCE FIRST, and that is not a
// detail — it is the reason this test can now catch a whole class it could not
// catch before. store.LatestMigrationVersion() is populated as a SIDE EFFECT of
// running migrations (migrations.go assigns it inside runVersionedMigrations),
// so it reads 0 in a process that never ran any. That used to be impossible:
// every process built its own template and therefore migrated. Once a template
// is shared across processes (see testdb.go's $SHINGO_TEST_PG path) the common
// case is a process that cloned a ready template and migrated nothing, where
// the bare comparison is 76 against 0 — and it would have been comparing 0
// against 0 in a run where the template was genuinely stale.
//
// So: re-open the clone through the production migrate path. Against an
// already-migrated database that is a no-op per migration (each checks
// schema_migrations and skips), but it builds the migration list, which is what
// publishes the head version.
func TestTemplateDB_HasAllSchema(t *testing.T) {
	db, cfg := OpenWithConfig(t)

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open clone through the migrate path: %v", err)
	}
	defer migrated.Close()

	var maxVersion int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&maxVersion); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	want := store.LatestMigrationVersion()
	if want == 0 {
		t.Fatal("store.LatestMigrationVersion() is 0 after a migrate run — the migration list published no head version")
	}
	if maxVersion != want {
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
		"bin_uop_ledger",
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
// template is fast. A regression indicates either schema bloat in the template
// or a Postgres lock-serialization problem under concurrency.
//
// A single wall-clock sample is load-sensitive — a busy CI runner (or, here, a
// concurrent docker build) inflates one clone and flakes the assertion. Instead
// take the MINIMUM of several clones: transient load slows some samples but not
// the fastest, which reflects the true clone cost (CREATE DATABASE ... TEMPLATE).
// A real regression in that cost still trips the bound; load noise doesn't. The
// threshold is kept generous to also absorb sustained moderate load.
//
// The first Open(t) warms the template (migrations) and is excluded.
func TestTemplateDB_CloneIsFast(t *testing.T) {
	_ = Open(t) // warm the template — first Open pays one-time setup cost.

	const samples = 5
	var best time.Duration
	for i := range samples {
		start := time.Now()
		_ = Open(t)
		d := time.Since(start)
		if i == 0 || d < best {
			best = d
		}
	}

	const threshold = time.Second
	if best > threshold {
		t.Errorf("fastest of %d template clones took %v, threshold is %v — investigate schema bloat or lock serialization", samples, best, threshold)
	} else {
		t.Logf("fastest template clone (%d samples): %v (threshold %v)", samples, best, threshold)
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

// TestTemplateDB_RanItsTests is the Sunday-smoke instrument's other half
// (fix-batch 2a): a test that can only pass by RUNNING, in the package the
// gate's docker step always visits.
//
// THE PROBLEM IT EXISTS FOR: `t.Skipf` on a docker failure makes `go test` exit
// 0 with every integration test skipped, and non-verbose output prints nothing
// per skipped test — so "green" and "Docker was down and 327 files ran nothing"
// are indistinguishable from the exit code. A smoke that only checks the exit
// code is a smoke that cannot see its own blindness.
//
// THE INTERESTING NUMBER LIVES ONE LEVEL UP, and this test does not pretend to
// hold it. Per-package test counts are `go test`'s own output — `ok
// shingocore/dispatch 22.8s` — and the gate log already carries them. What this
// test pins is the base of that chain from inside the run: the package the gate
// always visits had a live Open(), its counters moved, and (the part the smoke
// greps for) the sentinel was NOT emitted. A run where this passes and the
// sentinel fired is a contradiction, and the smoke treats it as one.
//
// MUTATION (verified): delete the Open() call. The count assertion fires — the
// counters did not move, which is the skip-everything shape wearing a green
// exit code.
func TestTemplateDB_RanItsTests(t *testing.T) {
	before := TestDatabasesCreated()
	db := Open(t)
	after := TestDatabasesCreated()
	if after <= before {
		t.Fatalf("TestDatabasesCreated did not advance (%d -> %d) — Open() cloned nothing, which is "+
			"the skip-everything shape: every docker test in this package skipped and the exit "+
			"code was still 0", before, after)
	}
	_ = db
}
