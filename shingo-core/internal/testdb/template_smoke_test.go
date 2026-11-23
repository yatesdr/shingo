//go:build docker

package testdb

import (
	"fmt"
	"sync"
	"sync/atomic"
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
// store.LatestMigrationVersion() is published at package init (it used to be
// a side effect of running migrations, which read 0 in a process that cloned
// a ready template and migrated nothing — the reason this test re-opened the
// clone through the migrate path first). The re-open is kept anyway: it also
// exercises the schema_migrations skip path against an already-migrated
// database, which is exactly what every template-cloning process does.
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

// TestTemplateDB_RanItsTests is the docker-down sentinel's other half
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

// TestAdminConn_OnePoolPerProcess: every CREATE and DROP DATABASE this process
// issues goes through one pool on the server's "postgres" database, not a new
// *sql.DB and a fresh backend each. adminConn used to open a pool per call: the
// levers study counted 9,046 admin backends in one docker step (28.6 ms of
// Postgres CPU each, work/loc-census-2026-11-20 REPORT-levers L1), one per
// create and one per drop. Cloning and dropping a database after the change
// must leave the same pool, holding at most 8 idle backends (adminIdle).
func TestAdminConn_OnePoolPerProcess(t *testing.T) {
	_ = Open(t)
	a, err := adminConn()
	testutilMustNoErr(t, err)
	b, err := adminConn()
	testutilMustNoErr(t, err)
	if a != b {
		t.Fatal("adminConn returned two pools in one process: each CREATE or DROP opens a backend of its own")
	}
	for i := 0; i < 3; i++ {
		t.Run(fmt.Sprintf("clone %d", i), func(t *testing.T) { _ = Open(t) })
	}
	c, err := adminConn()
	testutilMustNoErr(t, err)
	if c != a {
		t.Fatal("the pool changed across clones and drops")
	}
	if idle := c.Stats().Idle; idle > 8 {
		t.Errorf("the admin pool holds %d idle backends, want at most 8 (adminIdle)", idle)
	}
}

func testutilMustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestOpenSeeded_SeedsOncePerKeyAndEachCloneIsItsOwn: a key's Write runs once
// whether BuildSeeded or the first OpenSeeded builds it, its rows are in every
// clone, one clone's write is in no other, a second key writes its own template
// from the migrated one, and OpenSharedSeeded hands every holder of its key one
// database holding the seed's rows. SeededAge knows which templates are built.
func TestOpenSeeded_SeedsOncePerKeyAndEachCloneIsItsOwn(t *testing.T) {
	var mu sync.Mutex
	runs := map[string]int{}
	seed := func(key string) Seed {
		return Seed{Key: "selftest-seeded-" + key, Write: func(db *store.DB) error {
			mu.Lock()
			runs[key]++
			mu.Unlock()
			if _, err := db.Exec(`CREATE TABLE seeded_probe (v text)`); err != nil {
				return err
			}
			_, err := db.Exec(`INSERT INTO seeded_probe VALUES ($1)`, key)
			return err
		}}
	}
	holds := func(db *store.DB, want string) {
		t.Helper()
		var got string
		testutilMustNoErr(t, db.QueryRow(`SELECT string_agg(v, ',' ORDER BY v) FROM seeded_probe`).Scan(&got))
		if got != want {
			t.Errorf("the clone holds %q, want %q", got, want)
		}
	}

	a, b := seed("a"), seed("b")
	if _, ok := SeededAge(a); ok {
		t.Fatal("SeededAge says a template nobody built is built")
	}
	BuildSeeded(t, a)
	if age, ok := SeededAge(a); !ok || age < 0 {
		t.Fatalf("SeededAge after BuildSeeded = %v, %v; want built", age, ok)
	}
	first, _ := OpenSeeded(t, a)
	if _, err := first.Exec(`INSERT INTO seeded_probe VALUES ('z')`); err != nil {
		t.Fatalf("write to the first clone: %v", err)
	}
	second, _ := OpenSeeded(t, a)
	other, _ := OpenSeeded(t, b) // built here, by its first asker
	shared1, _ := OpenSharedSeeded(t, "selftest-seeded-shared", a)
	shared2, _ := OpenSharedSeeded(t, "selftest-seeded-shared", a)

	holds(first, "a,z")
	holds(second, "a")
	holds(other, "b")
	holds(shared1, "a")
	if shared1 != shared2 {
		t.Error("OpenSharedSeeded handed one key two databases")
	}
	if runs["a"] != 1 || runs["b"] != 1 {
		t.Errorf("seeds ran %v, want once a key", runs)
	}
}

// TestSeeded_ASerialSiblingEndsBeforeAParallelOneGoesOn pins the one fact a
// serial build test stands on (seeded.go, BUILT BEFORE THE LEAVES): a subtest
// that calls t.Parallel goes on only after its parent's function has returned,
// so a serial sibling declared after it has already ended. Top-level tests are
// the subtests of one root test (testing.runTests, go1.25.6), so a serial
// top-level test ends before any parallel one goes on, likewise.
func TestSeeded_ASerialSiblingEndsBeforeAParallelOneGoesOn(t *testing.T) {
	t.Parallel()
	var serialEnded atomic.Bool
	t.Run("parallel, declared first", func(t *testing.T) {
		t.Parallel()
		if !serialEnded.Load() {
			t.Error("a parallel subtest went on before its serial sibling ended, so a package's " +
				"serial BuildSeeded test no longer builds before its parallel tests start")
		}
	})
	t.Run("serial, declared second", func(t *testing.T) {
		serialEnded.Store(true)
	})
}
