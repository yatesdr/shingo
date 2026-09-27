//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// TestV147_OrdersPlan: orders gains plan, blank on every existing row, which
// acquiring reads as "pick", what every order before it did.
func TestV147_OrdersPlan(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	if !schema.ColumnExists(db.DB, "orders", "plan") {
		t.Fatal("orders.plan missing")
	}

	// Back to the pre-v147 shape, with a row written by an old Core.
	if _, err := db.Exec(`ALTER TABLE orders DROP COLUMN plan`); err != nil {
		t.Fatalf("drop plan: %v", err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity)
		VALUES ('v147-old', 'line-1', 'retrieve', 'queued', 1) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed a pre-v147 order: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 147`); err != nil {
		t.Fatalf("forget v147: %v", err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	defer migrated.Close()

	o, err := migrated.GetOrder(id)
	if err != nil {
		t.Fatalf("read the pre-v147 order: %v", err)
	}
	if o.Plan != nil {
		t.Errorf("the pre-v147 order carries plan %+v, want none", *o.Plan)
	}
}
