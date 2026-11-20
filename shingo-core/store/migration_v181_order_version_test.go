//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestOrderMigration_V181_EveryOrderStartsAtOne: v181 adds orders.version over
// orders written before the column, live and terminal, and every one reads 1,
// so no order Core holds can put a 0 on the wire, which a station reads as an
// older Core. Re-applied through the migrate path (the v136 pattern); a second
// open changes nothing and every post-condition holds. An order created after
// the column exists starts at 1 too.
func TestOrderMigration_V181_EveryOrderStartsAtOne(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	live := testdb.CreateOrder(t, db)
	ended := testdb.CreateOrder(t, db)
	testdb.SeedOrderStatus(t, db, ended.ID, "confirmed", "pin: an ended order")
	for _, stmt := range []string{
		`ALTER TABLE orders DROP COLUMN version`,
		`DELETE FROM schema_migrations WHERE version = 179`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v181 shape: "+stmt)
	}

	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v181")
		for _, id := range []int64{live.ID, ended.ID} {
			var v int64
			testutil.MustNoErr(t, migrated.QueryRow(`SELECT version FROM orders WHERE id=$1`, id).Scan(&v),
				"read the order")
			if v != 1 {
				t.Errorf("pass %d: order %d reads version %d, want 1", pass, id, v)
			}
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}

	born := testdb.CreateOrder(t, db)
	if got, err := db.GetOrder(born.ID); err != nil || got.Version != 1 {
		t.Errorf("an order born after v181 reads version %d (err %v), want 1", got.Version, err)
	}
}
