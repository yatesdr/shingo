//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// TestMigrate_V177_DepthOnlyInLanes: the depths a removed lane left behind are
// cleared by the rule the node writes hold now. SPR's shape: physical nodes
// directly under a group at depths 2-9 (the lane went, the depths stayed), a
// loose node with a depth, beside what must not move: a lane's slots, a flat
// position at 1, an unset depth, and the lane's own depth. Re-applied through
// the migrate path (the v136 pattern); a second open changes nothing.
func TestMigrate_V177_DepthOnlyInLanes(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	grp := testdb.MarkedGroup(t, db, "V177-GRP")
	lane, err := nodes.AddLane(db.DB, *grp, "V177-LANE")
	testutil.MustNoErr(t, err, "add lane")
	node := func(name string, parent *int64) int64 {
		n := &nodes.Node{Name: name, Enabled: true, ParentID: parent}
		testutil.MustNoErr(t, db.CreateNode(n), "create "+name)
		return n.ID
	}
	ids := map[string]int64{
		"left-9": node("V177-LEFT-9", grp), "left-2": node("V177-LEFT-2", grp),
		"flat-1": node("V177-FLAT-1", grp), "unset": node("V177-UNSET", grp),
		"loose": node("V177-LOOSE", nil), "slot-3": node("V177-SLOT-3", &lane),
	}
	// The rows as an older binary left them.
	for _, row := range []struct {
		id    int64
		depth int
	}{{ids["left-9"], 9}, {ids["left-2"], 2}, {ids["flat-1"], 1}, {ids["loose"], 4}, {ids["slot-3"], 3}, {lane, 2}} {
		_, err := db.Exec(`UPDATE nodes SET depth = $1 WHERE id = $2`, row.depth, row.id)
		testutil.MustNoErr(t, err, "set a pre-v177 depth")
	}
	_, err = db.Exec(`DELETE FROM schema_migrations WHERE version = 177`)
	testutil.MustNoErr(t, err, "back to before v177")
	want := map[int64]sql.NullInt64{
		ids["left-9"]: {Int64: 1, Valid: true}, ids["left-2"]: {Int64: 1, Valid: true},
		ids["flat-1"]: {Int64: 1, Valid: true}, ids["unset"]: {},
		ids["loose"]: {}, ids["slot-3"]: {Int64: 3, Valid: true},
		lane: {Int64: 2, Valid: true},
	}

	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v177")
		for id, w := range want {
			var got sql.NullInt64
			testutil.MustNoErr(t, migrated.QueryRow(`SELECT depth FROM nodes WHERE id = $1`, id).Scan(&got), "read depth")
			if got != w {
				t.Errorf("pass %d: node %d depth = %v, want %v", pass, id, got, w)
			}
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}
}
