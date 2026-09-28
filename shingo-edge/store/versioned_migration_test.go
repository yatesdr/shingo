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

	// Exactly the baseline row: the version table exists, holds one row, and
	// it is v1. A v2+ row would mean the runner applied something the
	// baseline already covers; zero rows would mean it never seeded.
	var rows, maxV int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&rows, &maxV); err != nil {
		t.Fatalf("schema_migrations not present after Open: %v", err)
	}
	if rows != 1 || maxV != 1 {
		t.Fatalf("fresh Open seeded %d rows (max v%d), want exactly 1 row at baseline v1", rows, maxV)
	}

	// Latest agrees — the wire's schema version for a fresh edge is the
	// baseline, read back through the same accessor the heartbeat uses.
	if v, err := migrate.Latest(db.DB, migrate.SQLite); err != nil || v != 1 {
		t.Fatalf("migrate.Latest = (%d, %v), want (1, nil)", v, err)
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
	if rows != 1 {
		t.Fatalf("aged DB seeded %d rows, want 1 (baseline only, chain not re-run)", rows)
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

	calls := 0
	list := []migrate.Migration{{
		Version: 2,
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

	// Baseline intact: v2 applied without disturbing v1.
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("schema_migrations rows = (%d, %v), want 2 (baseline v1 + v2)", rows, err)
	}
}
