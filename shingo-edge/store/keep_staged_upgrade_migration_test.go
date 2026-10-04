package store

import (
	"path/filepath"
	"testing"

	"shingoedge/store/schema"
)

// THE KEEP-STAGED FLAG IS DROPPED ON UPGRADE, AND NOTHING IS CARRIED.
//
// keep_staged armed a spot on a claim's inbound_staging. The keep-staged node is
// named on the claim now (keep_staged_node), and the flag goes through the
// claims table's rebuild. Its value is not carried into the name: the flags
// stored before keeping a staged spare was built were cleared by an earlier
// upgrade (migration 13), and nothing with keep-staged was ever deployed. A
// database that still has the column is rebuilt once, keeps every claim, and
// every claim reads keep-staged off.
func TestUpgradeDropsTheKeepStagedFlag(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// The database as a build with the flag left it: the column back, two
	// claims carrying it, and migration 13 not yet run.
	if _, err := db.Exec(`ALTER TABLE style_node_claims ADD COLUMN keep_staged INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatalf("put the flag column back: %v", err)
	}
	res, err := db.Exec(`INSERT INTO styles (name) VALUES ('UPG-STYLE')`)
	if err != nil {
		t.Fatalf("style: %v", err)
	}
	styleID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("style id: %v", err)
	}
	for _, node := range []string{"UPG-LINE-1", "UPG-LINE-2"} {
		if _, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, swap_mode, inbound_staging, keep_staged)
			VALUES (?, ?, 'two_robot', 'UPG-STG', 1)`, styleID, node); err != nil {
			t.Fatalf("claim %s: %v", node, err)
		}
	}
	version := 0
	for _, m := range edgeMigrations() {
		if m.Name == "clear_keep_staged_stored_before_the_design" {
			version = m.Version
		}
	}
	if version == 0 {
		t.Fatal("no Edge migration named clear_keep_staged_stored_before_the_design")
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version >= ?`, version); err != nil {
		t.Fatalf("roll the version table back: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The upgrade.
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen (the upgrade): %v", err)
	}
	defer db.Close()

	has, err := schema.TableHasColumn(db.DB, "style_node_claims", "keep_staged")
	if err != nil {
		t.Fatalf("read the columns: %v", err)
	}
	if has {
		t.Error("style_node_claims still has keep_staged after the upgrade")
	}
	var n, named int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(keep_staged_node <> ''), 0) FROM style_node_claims
		WHERE style_id = ?`, styleID).Scan(&n, &named); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 || named != 0 {
		t.Errorf("after the upgrade: %d claims, %d naming a keep-staged node; want 2 and 0", n, named)
	}

	// A later boot leaves the table alone, and a name set through a save stays.
	if _, err := db.Exec(`UPDATE style_node_claims SET keep_staged_node = 'UPG-STG' WHERE core_node_name = 'UPG-LINE-1'`); err != nil {
		t.Fatalf("name a spot after the upgrade: %v", err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM style_node_claims WHERE keep_staged_node <> ''`).Scan(&named); err != nil || named != 1 {
		t.Errorf("names after a later boot = (%d, %v), want the one set after the upgrade", named, err)
	}
}
