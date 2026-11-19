//go:build docker

package store_test

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// TestOrderMigration_V181_TheRuleRefusesANewBreakAndKeepsTheOldRows: v181 adds
// the rule that a `dispatched` order names its fleet job. A row already
// breaking it when the rule lands (`dispatched`, no vendor id: a hand that wrote
// `dispatched` before asking, under the old shape) is counted, logged and left
// as it is: the migration does not rewrite it and the rule, NOT VALID, does not
// reject it. Every new write is held to it: a vendorless `dispatched` write is
// refused, one naming its job lands. Re-applied through the migrate path; a
// second open changes nothing and every post-condition holds.
func TestOrderMigration_V181_TheRuleRefusesANewBreakAndKeepsTheOldRows(t *testing.T) {
	testdb.DisableWedgeSweep(t, "the pin writes the old shape (dispatched, no vendor id) the rule is added over")
	db, cfg := testdb.OpenWithConfig(t)
	old := testdb.CreateOrder(t, db)
	fresh := testdb.CreateOrder(t, db)
	for _, stmt := range []string{
		`ALTER TABLE orders DROP CONSTRAINT orders_dispatched_names_its_job`,
		`DELETE FROM schema_migrations WHERE version = 181`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v181 shape: "+stmt)
	}
	_, err := db.Exec(`UPDATE orders SET status='dispatched', vendor_order_id='' WHERE id=$1`, old.ID)
	testutil.MustNoErr(t, err, "the old shape, before the rule")

	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v181")
		var status, vendor string
		testutil.MustNoErr(t, migrated.QueryRow(`SELECT status, vendor_order_id FROM orders WHERE id=$1`, old.ID).
			Scan(&status, &vendor), "read the old row")
		if status != "dispatched" || vendor != "" {
			t.Errorf("pass %d: the old row reads %s vendor %q; v181 rewrote it", pass, status, vendor)
		}
		if !schema.ConstraintExists(migrated.DB, "orders", "orders_dispatched_names_its_job") {
			t.Errorf("pass %d: the rule is not on orders", pass)
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}

	_, err = db.Exec(`UPDATE orders SET status='dispatched', vendor_order_id='' WHERE id=$1`, fresh.ID)
	if err == nil || !strings.Contains(err.Error(), "orders_dispatched_names_its_job") {
		t.Errorf("a new vendorless `dispatched` write = %v, want the rule's refusal", err)
	}
	_, err = db.Exec(`UPDATE orders SET status='dispatched', vendor_order_id='sg-v181' WHERE id=$1`, fresh.ID)
	testutil.MustNoErr(t, err, "a `dispatched` write naming its job")
}
