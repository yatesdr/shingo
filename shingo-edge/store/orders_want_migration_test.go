package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
)

// Edge v30 (orders_want): the order row gains the height an empty is wanted
// at. A database the previous build left has no column; the upgrade adds it,
// every row it held states none, and the previous build's own INSERT (it names
// load_type and no want) still runs against the migrated table, so a
// rolled-back Edge keeps creating orders.
func TestV30_TheUpgradeAddsTheWantAndThePreviousBuildStillWrites(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "want.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	previousInsert := `INSERT INTO orders (uuid, order_type, process_node_id, retrieve_empty, quantity, delivery_node,
		staging_node, source_node, load_type, auto_confirm, payload_code, origin_id, origin_class)
		VALUES (?, 'retrieve', NULL, 1, 1, 'OUT', '', 'SUPER', 'standard', 1, 'PART', '', 'no_demand')`
	if _, err := db.Exec(previousInsert, "v30-before"); err != nil {
		t.Fatalf("the previous build's order: %v", err)
	}
	version := 0
	for _, m := range edgeMigrations() {
		if m.Name == "orders_want" {
			version = m.Version
		}
	}
	if version == 0 {
		t.Fatal("no Edge migration named orders_want")
	}
	for _, stmt := range []string{
		`ALTER TABLE orders DROP COLUMN want`,
		`DELETE FROM schema_migrations WHERE version >= 30`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("leave the previous build's shape (%s): %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	first := bootAndDump(t, path)
	if n := probeRaw(t, path, `SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name = 'want'`); n != 1 {
		t.Fatalf("after the upgrade orders has %d want columns, want 1", n)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	o, err := db.GetOrderByUUID("v30-before")
	if err != nil || o.Want != (protocol.Height{}) {
		t.Fatalf("the row the previous build wrote reads want %v (%v), want none", o.Want, err)
	}
	if _, err := db.Exec(previousInsert, "v30-rolled-back"); err != nil {
		t.Fatalf("the previous build's INSERT against the migrated table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if second := bootAndDump(t, path); second != first {
		t.Fatalf("the next boot changed the schema.\n%s", firstDumpDiff(first, second))
	}
	if version != 30 {
		t.Errorf("orders_want is v%d, want 30", version)
	}
}
