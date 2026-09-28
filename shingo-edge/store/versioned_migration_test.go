package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"shingo/protocol/migrate"
)

// Versioned-migration adoption pins (the protocol/migrate runner call at the
// tail of migrate()). The frozen chain above that call is pinned separately
// by frozen_chain_pin_test.go; these pin the runner's own behaviour on Edge.
//
// An AGED database here is a fully-frozen-chain database with the version
// table dropped — exactly the shape every plant DB has on the day adoption
// ships: schema current, no schema_migrations.
func TestOpenSeedsSchemaMigrationsBaseline_Fresh(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	defer db.Close()

	// Exactly the baseline row plus one row per listed migration: the
	// version table exists, v1 is seeded, and every edgeMigrations() entry
	// applied once. Zero rows would mean it never seeded; an extra row would
	// mean something applied that is not in the list.
	ms := edgeMigrations()
	wantRows, wantMax := 1+len(ms), edgeBaselineVersion
	if len(ms) > 0 {
		wantMax = migrate.LatestVersion(ms)
	}
	var rows, maxV int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&rows, &maxV); err != nil {
		t.Fatalf("schema_migrations not present after Open: %v", err)
	}
	if rows != wantRows || maxV != wantMax {
		t.Fatalf("fresh Open recorded %d rows (max v%d), want %d rows (baseline v1 + the list) at v%d",
			rows, maxV, wantRows, wantMax)
	}

	// Latest agrees — the wire's schema version for a fresh edge, read back
	// through the same accessor the heartbeat uses.
	if v, err := migrate.Latest(db.DB, migrate.SQLite); err != nil || v != wantMax {
		t.Fatalf("migrate.Latest = (%d, %v), want (%d, nil)", v, err, wantMax)
	}
}

// The plant path: an aged DB (frozen chain done, version table absent) is
// ADOPTED at baseline — seeded once, data untouched, chain not re-run.
func TestOpenSeedsSchemaMigrationsBaseline_Aged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Plant rows so "data survives" is a real assertion.
	if _, err := db.Exec(`INSERT INTO styles (name) VALUES ('proof-style')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("simulate pre-adoption aged DB: %v", err)
	}
	db.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("aged re-open under adoption: %v", err)
	}
	defer again.Close()

	var rows int
	if err := again.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("aged DB got no schema_migrations: %v", err)
	}
	// Baseline seeded once, then every listed migration applied once. Each
	// is written to be safe on a DB where the work is already done, which is
	// exactly this DB's shape.
	if want := 1 + len(edgeMigrations()); rows != want {
		t.Fatalf("aged DB recorded %d rows, want %d (baseline + the list, chain not re-run)", rows, want)
	}
	var n int
	if err := again.QueryRow(`SELECT COUNT(*) FROM styles WHERE name='proof-style'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("aged DB data lost: styles proof-style = (%d, %v), want (1, nil)", n, err)
	}
}

// Apply-once: a version > baseline migration runs exactly once across two
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
	next := migrate.LatestVersion(edgeMigrations()) + 1
	if next <= edgeBaselineVersion {
		next = edgeBaselineVersion + 1
	}
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
	want := 1 + len(edgeMigrations()) + 1
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != want {
		t.Fatalf("schema_migrations rows = (%d, %v), want %d (baseline + the list + the synthetic one)", rows, err, want)
	}
}

// edgeMigrationVersion returns the version edgeMigrations() gives the named
// migration. Tests find a migration's row by its name, never by a literal
// version, so renumbering on a rebase touches the list and nothing else.
func edgeMigrationVersion(t *testing.T, name string) int {
	t.Helper()
	for _, m := range edgeMigrations() {
		if m.Name == name {
			return m.Version
		}
	}
	t.Fatalf("no Edge migration named %q", name)
	return 0
}

// TestEdgeMigrations_Shape pins the list by structure, not by numbers:
// versions strictly increasing, the first above the adopted baseline (the
// baseline row stands for the whole frozen chain), and LatestVersion is the
// last entry.
func TestEdgeMigrations_Shape(t *testing.T) {
	ms := edgeMigrations()
	if err := migrate.CheckOrder(ms); err != nil {
		t.Error(err)
	}
	if len(ms) == 0 {
		return
	}
	if ms[0].Version <= edgeBaselineVersion {
		t.Errorf("first Edge migration is v%d, want above the baseline v%d", ms[0].Version, edgeBaselineVersion)
	}
	if got, last := migrate.LatestVersion(ms), ms[len(ms)-1].Version; got != last {
		t.Errorf("LatestVersion = %d, want the last entry's %d", got, last)
	}
}
