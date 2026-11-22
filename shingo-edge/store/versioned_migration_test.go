package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"shingo/protocol/migrate"
)

// The versioned runner on the Edge (protocol/migrate, above RetiredChain).
// What a start does with the files it is handed is boot_schema_pins_test.go's.

// Apply-once: a version above the retired chain runs exactly once across two
// runVersioned invocations — the runner's exists-check is the gate, not the
// body's own idempotency (the body below would happily run again).
func TestRunVersionedAppliesExactlyOnce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "once.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	// The synthetic migration takes the next free version, so it is new to
	// this DB whatever the real list holds.
	calls := 0
	next := latestVersion() + 1
	list := []migrate.Migration{{
		Version: next,
		Name:    "test_marker",
		Fn: func(tx *sql.Tx) error {
			calls++
			_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS versioned_marker (id INTEGER PRIMARY KEY)`)
			return err
		},
	}}

	if err := db.runVersioned(list); err != nil {
		t.Fatalf("first runVersioned: %v", err)
	}
	if calls != 1 {
		t.Fatalf("body ran %d times on first call, want 1", calls)
	}

	// Second call: exists-check must skip. A re-run here would mean the gate
	// was the body's idempotency, not the runner's.
	if err := db.runVersioned(list); err != nil {
		t.Fatalf("second runVersioned: %v", err)
	}
	if calls != 1 {
		t.Fatalf("body ran %d times across two calls, want 1 — the runner's exists-check is the gate", calls)
	}

	// Earlier rows intact: the synthetic version applied without disturbing
	// the baseline or the real list.
	want := RetiredChain + len(edgeMigrations()) + 1
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != want {
		t.Fatalf("schema_migrations rows = (%d, %v), want %d (baseline + the list + the synthetic one)", rows, err, want)
	}
}

// TestEdgeMigrations_Shape pins the list by structure, not by numbers:
// versions strictly increasing, the first above the retired chain (the
// baseline records 1..RetiredChain), and LatestVersion is the last entry.
func TestEdgeMigrations_Shape(t *testing.T) {
	ms := edgeMigrations()
	if err := migrate.CheckOrder(ms); err != nil {
		t.Error(err)
	}
	if len(ms) == 0 {
		return
	}
	if ms[0].Version <= RetiredChain {
		t.Errorf("first Edge migration is v%d, want above the retired chain v%d", ms[0].Version, RetiredChain)
	}
	if got, last := migrate.LatestVersion(ms), ms[len(ms)-1].Version; got != last {
		t.Errorf("LatestVersion = %d, want the last entry's %d", got, last)
	}
}

// TestRunVersioned_AFailedReRunKeepsTheVersionRecorded pins the runner's
// self-heal on SQLite: a version recorded as applied whose post-condition then
// fails is re-run, and a re-run that fails leaves the version recorded, since
// the stale row is deleted inside the re-run's transaction (migrate.Run). It
// used to be deleted in a statement of its own first, so a failed re-run left
// the version unrecorded. Core runs the same runner on Postgres
// (TestMigrate_AFailedReRunKeepsTheVersionRecorded).
func TestRunVersioned_AFailedReRunKeepsTheVersionRecorded(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "rerun.db"))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	next := latestVersion() + 1
	healthy := true
	list := []migrate.Migration{{
		Version: next,
		Name:    "test_rerun",
		Fn: func(tx *sql.Tx) error {
			if !healthy {
				return errors.New("the re-run fails")
			}
			_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS rerun_marker (id INTEGER PRIMARY KEY)`)
			return err
		},
		Verify: func(migrate.Querier) bool { return healthy },
	}}
	recorded := func() bool {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, next).Scan(&n); err != nil {
			t.Fatalf("read schema_migrations: %v", err)
		}
		return n == 1
	}

	if err := db.runVersioned(list); err != nil {
		t.Fatalf("first runVersioned: %v", err)
	}
	if !recorded() {
		t.Fatalf("v%d is not recorded after it applied", next)
	}
	healthy = false
	if err := db.runVersioned(list); err == nil {
		t.Fatal("the re-run failed but runVersioned returned no error")
	}
	if !recorded() {
		t.Errorf("v%d is no longer recorded after its re-run failed: the stale row was deleted outside the "+
			"re-run's transaction, so migrate check and migrate down lose a version that was applied", next)
	}
}
