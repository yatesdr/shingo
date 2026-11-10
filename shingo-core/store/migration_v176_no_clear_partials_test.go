//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
)

// TestLoaderMigration_V176_EveryLoaderKeepsTheClear: v176 adds
// bin_loaders.no_clear_partials over loaders written before the column, live
// and archived, and every one reads false, the clear every dedicated loader
// has, so no board changes on the deploy. Re-applied through the migrate path
// (the v136 pattern); a second open changes nothing and every post-condition
// holds.
func TestLoaderMigration_V176_EveryLoaderKeepsTheClear(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	var ids []int64
	for _, name := range []string{"V176-LIVE", "V176-ARCHIVED"} {
		id, err := db.CreateLoader(loaders.Loader{Name: name, Role: loaders.RoleProduce,
			Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentOperator})
		testutil.MustNoErr(t, err, "seed "+name)
		ids = append(ids, id)
	}
	testutil.MustNoErr(t, db.DeleteLoader(ids[1]), "archive V176-ARCHIVED")
	for _, stmt := range []string{
		`ALTER TABLE bin_loaders DROP COLUMN no_clear_partials`,
		`DELETE FROM schema_migrations WHERE version = 176`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v176 shape: "+stmt)
	}

	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v176")
		for _, id := range ids {
			var off bool
			testutil.MustNoErr(t, migrated.QueryRow(`SELECT no_clear_partials FROM bin_loaders WHERE id=$1`, id).Scan(&off),
				"read the loader")
			if off {
				t.Errorf("pass %d: loader %d reads no_clear_partials true, want false: the clear stays on", pass, id)
			}
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}
}
