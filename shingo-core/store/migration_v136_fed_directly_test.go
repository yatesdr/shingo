//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
)

// TestLoaderMigration_FedDirectlyBackfillMatchesBlankInbound: v136 adds
// bin_loaders.fed_directly and backfills it true where inbound_source is blank — exactly
// the loaders that pull nothing today — so no pre-existing loader's pulling
// changes, and every inbound source survives as it was. Re-applied through the
// migrate path (the v94/v119 pattern) over rows written before the column.
func TestLoaderMigration_FedDirectlyBackfillMatchesBlankInbound(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	type row struct {
		name, inbound string
		archived      bool
	}
	seed := []row{
		{"V136-DIRECT", "", false},
		{"V136-PULLS", "SOME-GROUP", false},
		{"V136-OLD-DIRECT", "", true},
		{"V136-OLD-PULLS", "OTHER-GROUP", true},
	}
	ids := map[string]int64{}
	for _, r := range seed {
		id, err := db.CreateLoader(loaders.Loader{
			Name: r.name, Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow,
			Replenishment: loaders.ReplenishmentOperator, InboundSource: r.inbound,
		})
		testutil.MustNoErr(t, err, "seed "+r.name)
		if r.archived {
			testutil.MustNoErr(t, db.DeleteLoader(id), "archive "+r.name)
		}
		ids[r.name] = id
	}

	for _, stmt := range []string{
		`ALTER TABLE bin_loaders DROP COLUMN fed_directly`,
		`DELETE FROM schema_migrations WHERE version = 136`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v136 shape: "+stmt)
	}
	migrated, err := store.Open(cfg)
	testutil.MustNoErr(t, err, "re-open to apply v136")
	defer migrated.Close()

	for _, r := range seed {
		var fed bool
		var inbound string
		testutil.MustNoErr(t, migrated.QueryRow(`SELECT fed_directly, inbound_source FROM bin_loaders WHERE id=$1`,
			ids[r.name]).Scan(&fed, &inbound), "read "+r.name)
		if fed != (r.inbound == "") || inbound != r.inbound {
			t.Errorf("%s: fed_directly %v inbound %q, want %v and %q unchanged", r.name, fed, inbound, r.inbound == "", r.inbound)
		}
	}
}
