//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// TestV146_OrdersProjectsTo: orders gains projects_to, blank on every existing
// row. A station-known row never reads it, so every order before it sends what
// it sent.
func TestV146_OrdersProjectsTo(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	if !schema.ColumnExists(db.DB, "orders", "projects_to") {
		t.Fatal("orders.projects_to missing")
	}

	// Back to the pre-v146 shape, with a row written by an old Core.
	if _, err := db.Exec(`ALTER TABLE orders DROP COLUMN projects_to`); err != nil {
		t.Fatalf("drop projects_to: %v", err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity)
		VALUES ('v146-old', 'line-1', 'retrieve', 'queued', 1) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed a pre-v146 order: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 146`); err != nil {
		t.Fatalf("forget v146: %v", err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	defer migrated.Close()

	o, err := migrated.GetOrder(id)
	if err != nil {
		t.Fatalf("read the pre-v146 order: %v", err)
	}
	if o.ProjectsTo != "" {
		t.Errorf("the pre-v146 order projects to %q, want blank", o.ProjectsTo)
	}
}
