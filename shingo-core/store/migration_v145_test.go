//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// TestV145_OrdersStationUnaware: orders gains station_unaware, false on every
// existing row, which is what every order was before B2: known to its station.
func TestV145_OrdersStationUnaware(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	if !schema.ColumnExists(db.DB, "orders", "station_unaware") {
		t.Fatal("orders.station_unaware missing")
	}

	// Back to the pre-v145 shape, with a row written by an old Core.
	if _, err := db.Exec(`ALTER TABLE orders DROP COLUMN station_unaware`); err != nil {
		t.Fatalf("drop station_unaware: %v", err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity)
		VALUES ('v145-old', 'line-1', 'retrieve', 'queued', 1) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed a pre-v145 order: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 145`); err != nil {
		t.Fatalf("forget v145: %v", err)
	}
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	defer migrated.Close()

	o, err := migrated.GetOrder(id)
	if err != nil {
		t.Fatalf("read the pre-v145 order: %v", err)
	}
	if o.StationUnaware {
		t.Error("the pre-v145 order reads station_unaware, want false: its station knew it")
	}
}
