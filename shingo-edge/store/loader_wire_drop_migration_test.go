package store

import (
	"path/filepath"
	"testing"
)

// Edge v27 (drop_loader_carrier_mix_and_position_kind): the two carrier-mix
// tables and core_loader_positions.kind leave the schema, and
// core_loaders.config_gen stays for a rolled-back build.

// droppedLoaderObjects counts how many of the three objects v27 drops exist.
const droppedLoaderObjects = `SELECT
	(SELECT COUNT(*) FROM sqlite_master WHERE type='table'
		AND name IN ('core_loader_window_bin_types', 'core_loader_quotas'))
	+ (SELECT COUNT(*) FROM pragma_table_info('core_loader_positions') WHERE name = 'kind')`

// previousBuildCreates are the three frozen-chain statements the previous
// build (cbb37955) runs on every boot: a rolled-back Edge re-creates what v27
// dropped.
var previousBuildCreates = []string{
	`ALTER TABLE core_loader_positions ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE IF NOT EXISTS core_loader_window_bin_types (
		loader_key TEXT NOT NULL, position_node TEXT NOT NULL, bin_type_code TEXT NOT NULL,
		PRIMARY KEY (loader_key, position_node, bin_type_code))`,
	`CREATE TABLE IF NOT EXISTS core_loader_quotas (
		loader_key TEXT NOT NULL, bin_type_code TEXT NOT NULL, want INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (loader_key, bin_type_code))`,
}

// TestV27_FreshDatabaseHasNeitherTableNorKind: a fresh database has none of
// the three, and two boots leave identical schemas.
func TestV27_FreshDatabaseHasNeitherTableNorKind(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	if n := probeRaw(t, path, droppedLoaderObjects); n != 0 {
		t.Fatalf("a fresh database has %d of the dropped loader objects, want 0", n)
	}
	if second := bootAndDump(t, path); second != first {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
}

// TestV27_AgedDatabaseDropsThemAndTheNextBootLeavesThemDropped: a database a
// previous build left (the three objects, with rows; no schema_migrations)
// loses all three on boot, keeps its loader rows, and the next boot changes
// nothing.
func TestV27_AgedDatabaseDropsThemAndTheNextBootLeavesThemDropped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	plant := append(append([]string{}, previousBuildCreates...),
		`INSERT INTO core_loaders (loader_key, role, config_gen) VALUES ('loader:1', 'produce', 4)`,
		`INSERT INTO core_loader_positions (loader_key, position_node, payload_code, kind) VALUES ('loader:1', 'W1', '', 'window')`,
		`INSERT INTO core_loader_window_bin_types VALUES ('loader:1', 'W1', 'TOTE-1')`,
		`INSERT INTO core_loader_quotas VALUES ('loader:1', 'TOTE-1', 3)`,
		`DROP TABLE schema_migrations`,
	)
	for _, stmt := range plant {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("plant the previous build's shape: %v", err)
		}
	}
	db.Close()

	first := bootAndDump(t, path)
	if n := probeRaw(t, path, droppedLoaderObjects); n != 0 {
		t.Fatalf("%d of the dropped loader objects survived the boot", n)
	}
	if n := probeRaw(t, path, `SELECT COUNT(*) FROM core_loader_positions WHERE loader_key = 'loader:1'`); n != 1 {
		t.Fatalf("the position row was lost with the column: %d rows", n)
	}
	if second := bootAndDump(t, path); second != first {
		t.Fatalf("the next boot re-created what v27 dropped.\n%s", firstDumpDiff(first, second))
	}
}

// TestV27_ThePreviousBuildsLoaderStatementsStillRun: the three statements of
// the previous build's loader sync and loader reads that name
// core_loaders.config_gen (store/core_loaders.go at cbb37955) run against the
// migrated schema, so a rolled-back Edge still starts with its loaders.
func TestV27_ThePreviousBuildsLoaderStatementsStillRun(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO core_loaders (loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, config_gen, funnel_windows, changeover_load_directive, leaves_bare, auto_push, pulled_directly, synced_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,datetime('now'))`,
		"loader:7", "produce", "L7", "shared_window", "operator", "FG", "SM", 3, false, false, false, false, false); err != nil {
		t.Fatalf("the previous build's loader insert: %v", err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, config_gen, funnel_windows, changeover_load_directive, leaves_bare, auto_push, pulled_directly FROM core_loaders ORDER BY loader_key`, nil},
		{`SELECT loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, config_gen, funnel_windows, changeover_load_directive, leaves_bare, auto_push, pulled_directly FROM core_loaders WHERE loader_key=?`, []any{"loader:7"}},
	} {
		rows, err := db.Query(q.sql, q.args...)
		if err != nil {
			t.Fatalf("the previous build's loader read: %v", err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 1 {
			t.Errorf("the previous build's loader read returned %d rows, want 1", n)
		}
	}
}

// TestV27_RolledBackThenForward: a rolled-back Edge's boot re-creates the two
// tables and kind (its frozen chain still has the steps); rolled forward
// again, v27 is already applied and does not run twice, so they stay, empty
// and unread. Accepted: nothing reads or writes them.
func TestV27_RolledBackThenForward(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rollback.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range previousBuildCreates {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("the rolled-back build's boot: %v", err)
		}
	}
	db.Close()
	bootAndDump(t, path)
	if n := probeRaw(t, path, droppedLoaderObjects); n != 3 {
		t.Fatalf("rolled forward: %d of the three objects remain, want all 3 (v27 does not run twice)", n)
	}
}
