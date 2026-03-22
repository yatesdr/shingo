package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenMigratesLegacyOrdersWithoutOpNodeID(t *testing.T) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer raw.Close()

	legacySchema := `
CREATE TABLE orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid            TEXT NOT NULL UNIQUE,
    order_type      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    retrieve_empty  INTEGER NOT NULL DEFAULT 1,
    quantity        INTEGER NOT NULL DEFAULT 0,
    delivery_node   TEXT NOT NULL DEFAULT '',
    staging_node    TEXT NOT NULL DEFAULT '',
    pickup_node     TEXT NOT NULL DEFAULT '',
    load_type       TEXT NOT NULL DEFAULT '',
    waybill_id      TEXT,
    external_ref    TEXT,
    final_count     INTEGER,
    count_confirmed INTEGER NOT NULL DEFAULT 0,
    eta             TEXT,
    auto_confirm    INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_orders_status ON orders(status);
CREATE INDEX idx_orders_uuid ON orders(uuid);
`
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name='op_node_id'`).Scan(&count); err != nil {
		t.Fatalf("check op_node_id column: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected op_node_id column to exist after migration, count=%d", count)
	}

	if _, err := db.Exec(`INSERT INTO orders (uuid, order_type, op_node_id) VALUES ('uuid-test', 'retrieve', NULL)`); err != nil {
		t.Fatalf("insert migrated order with op_node_id column: %v", err)
	}
}
