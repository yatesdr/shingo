package store

import (
	"path/filepath"
	"testing"
)

// A keep_staged flag stored before keeping a staged spare was built would
// switch its line to the short swap the moment the new code started, on a spot
// nobody chose for it and no save had checked. The upgrade clears every flag
// already stored; flags set after the upgrade are left alone.
func TestUpgradeClearsKeepStagedStoredBeforeTheDesign(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	res, err := db.Exec(`INSERT INTO styles (name) VALUES ('UPG-STYLE')`)
	if err != nil {
		t.Fatalf("style: %v", err)
	}
	styleID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("style id: %v", err)
	}
	for _, node := range []string{"UPG-LINE-1", "UPG-LINE-2"} {
		if _, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, swap_mode, keep_staged)
			VALUES (?, ?, 'two_robot', 1)`, styleID, node); err != nil {
			t.Fatalf("claim %s: %v", node, err)
		}
	}

	// The database as it stood before the upgrade: the migration not yet run.
	name := "clear_keep_staged_stored_before_the_design"
	var version int
	for _, m := range edgeMigrations() {
		if m.Name == name {
			version = m.Version
		}
	}
	if version == 0 {
		t.Fatalf("no Edge migration named %q: a stored keep_staged flag survives the upgrade", name)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version >= ?`, version); err != nil {
		t.Fatalf("roll the version table back: %v", err)
	}
	if err := db.runVersioned(edgeMigrations()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM style_node_claims WHERE keep_staged <> 0`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d claims still keep a staged spare after the upgrade, want 0", n)
	}

	// After the upgrade a flag is the new design's, set through a checked save,
	// and a later boot leaves it.
	if _, err := db.Exec(`UPDATE style_node_claims SET keep_staged = 1 WHERE core_node_name = 'UPG-LINE-1'`); err != nil {
		t.Fatalf("set after the upgrade: %v", err)
	}
	if err := db.runVersioned(edgeMigrations()); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM style_node_claims WHERE keep_staged <> 0`).Scan(&n); err != nil || n != 1 {
		t.Errorf("flags after a later boot = (%d, %v), want the one set after the upgrade", n, err)
	}
}
