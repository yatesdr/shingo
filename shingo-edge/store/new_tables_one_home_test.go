package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"shingoedge/store/schema"
)

// Edge v40 (catid_holds) and v41 (line_counters). A new table has one home,
// the versioned chain, as v5 and v12: schema.Apply runs before the chain on
// fresh and aged databases alike, so a baseline copy made the chain's CREATE
// dead everywhere and left two copies free to drift.

const newTablesProbe = `SELECT COUNT(*) FROM sqlite_master
	WHERE (type = 'table' AND name IN ('catid_holds', 'line_counters'))
	   OR (type = 'index' AND name = 'idx_catid_holds_open')`

// TestOneHome_TheBaselineCreatesNeitherNewTable: the baseline DDL alone, on an
// empty database, creates neither table nor the holds' index.
func TestOneHome_TheBaselineCreatesNeitherNewTable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "baseline.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if err := schema.Apply(raw); err != nil {
		t.Fatalf("apply the baseline: %v", err)
	}
	raw.Close()
	if n := probeRaw(t, path, newTablesProbe); n != 0 {
		t.Errorf("the baseline created %d of catid_holds, its index and line_counters; want 0, the chain is their home", n)
	}
}

// TestOneHome_FreshAndV36DatabasesGetThemFromTheChain: a fresh database and
// one at v36 (neither table, the chain recorded through 36) both boot with
// both tables, the index, and v40 and v41 recorded.
func TestOneHome_FreshAndV36DatabasesGetThemFromTheChain(t *testing.T) {
	t.Parallel()
	const recorded = `SELECT COUNT(*) FROM schema_migrations WHERE version IN (40, 41)`
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	bootAndDump(t, fresh)
	if n := probeRaw(t, fresh, newTablesProbe); n != 3 {
		t.Errorf("fresh database: %d of the three objects, want 3", n)
	}
	if n := probeRaw(t, fresh, recorded); n != 2 {
		t.Errorf("fresh database: %d of v40 and v41 recorded, want 2", n)
	}

	aged := filepath.Join(t.TempDir(), "v36.db")
	bootAndDump(t, aged)
	raw, err := sql.Open("sqlite", aged)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	for _, stmt := range []string{
		`DROP INDEX idx_catid_holds_open`,
		`DROP TABLE catid_holds`,
		`DROP TABLE line_counters`,
		`ALTER TABLE process_nodes DROP COLUMN line_counter_id`,
		`DELETE FROM schema_migrations WHERE version > 36`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("plant the v36 shape (%s): %v", stmt, err)
		}
	}
	raw.Close()
	bootAndDump(t, aged)
	if n := probeRaw(t, aged, newTablesProbe); n != 3 {
		t.Errorf("v36 database: %d of the three objects after the boot, want 3", n)
	}
	if n := probeRaw(t, aged, recorded); n != 2 {
		t.Errorf("v36 database: %d of v40 and v41 recorded, want 2", n)
	}
}
