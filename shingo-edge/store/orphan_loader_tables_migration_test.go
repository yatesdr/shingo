package store

import (
	"path/filepath"
	"testing"
)

// Edge v2 (drop_orphan_loader_tables) on every shape a plant can have. The
// two tables have no DDL in schema.Apply and no reader or writer in any
// module, so only an upgraded Edge still carries them; v2 is what makes an
// upgraded database match a fresh one (the schemadump convergence test's
// half of the proof).

// orphanTableCount counts how many of the two dropped tables exist.
func orphanTableCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'
		AND name IN ('home_location_loaders', 'loader_payload_thresholds')`).Scan(&n); err != nil {
		t.Fatalf("count orphan tables: %v", err)
	}
	return n
}

func schemaRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

// plantOrphans puts both tables, with rows, onto an open DB — the shape a
// plant Edge has on the day v2 ships.
func plantOrphans(t *testing.T, db *DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE home_location_loaders (loader_key TEXT PRIMARY KEY)`,
		`INSERT INTO home_location_loaders (loader_key) VALUES ('L1')`,
		`CREATE TABLE loader_payload_thresholds (loader_key TEXT, payload_code TEXT, uop_threshold INTEGER,
			PRIMARY KEY (loader_key, payload_code))`,
		`INSERT INTO loader_payload_thresholds VALUES ('L1', 'P1', 10)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("plant orphan tables: %v", err)
		}
	}
}

// The plant path: an aged DB (no schema_migrations) carrying both tables with
// rows. The first boot drops them; the second boot applies nothing.
func TestV2DropsOrphanLoaderTables_AgedThenSecondBoot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	plantOrphans(t, db)
	if _, err := db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("simulate pre-adoption DB: %v", err)
	}
	db.Close()

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if n := orphanTableCount(t, first); n != 0 {
		t.Fatalf("after the first boot %d orphan table(s) remain, want 0", n)
	}
	after := schemaRows(t, first)
	if want := 1 + len(edgeMigrations()); after != want {
		t.Fatalf("first boot recorded %d schema_migrations rows, want %d", after, want)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	defer second.Close()
	if n := orphanTableCount(t, second); n != 0 {
		t.Fatalf("the second boot brought back %d orphan table(s) — something in the frozen chain recreates them", n)
	}
	if got := schemaRows(t, second); got != after {
		t.Fatalf("the second boot changed schema_migrations from %d to %d rows, want no change", after, got)
	}
}

// A DB where the work is already done (neither table ever existed, or a hand
// drop got there first) and v2 is not recorded: v2 is a successful no-op.
func TestV2DropsOrphanLoaderTables_NoOpWhenAlreadyGone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "done.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("simulate pre-adoption DB: %v", err)
	}
	db.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("boot on an already-clean DB: %v", err)
	}
	defer again.Close()
	var applied bool
	if err := again.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = ?)`,
		edgeMigrationVersion(t, "drop_orphan_loader_tables")).Scan(&applied); err != nil || !applied {
		t.Fatalf("v2 recorded = (%v, %v), want (true, nil)", applied, err)
	}
	if n := orphanTableCount(t, again); n != 0 {
		t.Fatalf("%d orphan table(s) on a DB that never had them", n)
	}
}

// v2 recorded, but a table has reappeared (a restored backup, a hand
// CREATE): Verify fails and the runner re-drops it.
func TestV2DropsOrphanLoaderTables_SelfHealsWhenRecordedButPresent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	plantOrphans(t, db)
	db.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer again.Close()
	if n := orphanTableCount(t, again); n != 0 {
		t.Fatalf("%d orphan table(s) survived a boot with v2 already recorded, want 0 (Verify self-heal)", n)
	}
}
