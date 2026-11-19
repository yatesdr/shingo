//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestOrderMigration_V180_NoOrderIsAsking: v180 adds orders.asked_vendor_order_id
// over orders written before the column, live and terminal, and every one reads
// blank: no order is being asked for at the deploy, so no reader counts one as
// spoken for that it did not before. Re-applied through the migrate path (the
// v136 pattern); a second open changes nothing and every post-condition holds.
// An order born after the column reads blank too, and reads back what a write
// puts there.
func TestOrderMigration_V180_NoOrderIsAsking(t *testing.T) {
	testdb.ExpectAskLeftOpen(t, "the pin writes an ask to read it back")
	db, cfg := testdb.OpenWithConfig(t)
	live := testdb.CreateOrder(t, db)
	ended := testdb.CreateOrder(t, db)
	testdb.SeedOrderStatus(t, db, ended.ID, "confirmed", "pin: an ended order")
	for _, stmt := range []string{
		`ALTER TABLE orders DROP COLUMN asked_vendor_order_id`,
		`DELETE FROM schema_migrations WHERE version = 180`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v180 shape: "+stmt)
	}

	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v180")
		for _, id := range []int64{live.ID, ended.ID} {
			var asked string
			testutil.MustNoErr(t, migrated.QueryRow(`SELECT asked_vendor_order_id FROM orders WHERE id=$1`, id).Scan(&asked),
				"read the order")
			if asked != "" {
				t.Errorf("pass %d: order %d reads ask %q, want blank", pass, id, asked)
			}
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}

	born := testdb.CreateOrder(t, db)
	if got, err := db.GetOrder(born.ID); err != nil || got.AskedVendorOrderID != "" {
		t.Fatalf("an order born after v180 reads ask %q (err %v), want blank", got.AskedVendorOrderID, err)
	}
	_, err := db.Exec(`UPDATE orders SET asked_vendor_order_id='sg-pin-1' WHERE id=$1`, born.ID)
	testutil.MustNoErr(t, err, "write an ask")
	if got, err := db.GetOrder(born.ID); err != nil || got.AskedVendorOrderID != "sg-pin-1" {
		t.Errorf("the order reads ask %q (err %v) after one was written, want sg-pin-1", got.AskedVendorOrderID, err)
	}
}
