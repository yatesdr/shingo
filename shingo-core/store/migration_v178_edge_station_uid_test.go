//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
)

// TestLoaderMigration_V178_EveryLoaderPicksNoEdge: v178 adds
// bin_loaders.edge_station_uid over loaders written before the column, live
// and archived, and every one reads blank, which picks no Edge, so no Edge
// changes what it makes on the deploy. Re-applied through the migrate path
// (the v136 pattern); a second open changes nothing and every post-condition
// holds.
func TestLoaderMigration_V178_EveryLoaderPicksNoEdge(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	var ids []int64
	for _, name := range []string{"V178-LIVE", "V178-ARCHIVED"} {
		id, err := db.CreateLoader(loaders.Loader{Name: name, Role: loaders.RoleProduce,
			Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator})
		testutil.MustNoErr(t, err, "seed "+name)
		ids = append(ids, id)
	}
	testutil.MustNoErr(t, db.DeleteLoader(ids[1]), "archive V178-ARCHIVED")
	for _, stmt := range []string{
		`ALTER TABLE bin_loaders DROP COLUMN edge_station_uid`,
		`DELETE FROM schema_migrations WHERE version = 178`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v178 shape: "+stmt)
	}

	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v178")
		for _, id := range ids {
			var uid string
			testutil.MustNoErr(t, migrated.QueryRow(`SELECT edge_station_uid FROM bin_loaders WHERE id=$1`, id).Scan(&uid),
				"read the loader")
			if uid != "" {
				t.Errorf("pass %d: loader %d reads edge_station_uid %q, want blank: no Edge picked", pass, id, uid)
			}
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}
}
