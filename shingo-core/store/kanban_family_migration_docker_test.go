//go:build docker

package store_test

import (
	"strconv"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// kanban_family_migration_docker_test.go — v172 on a database as it stood before it: groups by
// what a type says of itself and decides nothing else, and a second pass changes nothing.

func ageFamilySchema(t *testing.T, db *store.DB) {
	t.Helper()
	for _, s := range []string{
		`DROP TRIGGER IF EXISTS bin_types_family_of_one ON bin_types`,
		`DROP FUNCTION IF EXISTS bin_types_family_of_one()`,
		`DROP TABLE kanban_families`,
		`ALTER TABLE bin_types DROP COLUMN family_id`,
		`DROP TABLE carrier_families`,
		`ALTER TABLE kanbans ADD COLUMN running_cover INTEGER NOT NULL DEFAULT 0, ADD COLUMN overflow_node_id BIGINT REFERENCES nodes(id) ON DELETE SET NULL`,
		`ALTER TABLE kanban_levels ADD COLUMN overflow_node_id BIGINT REFERENCES nodes(id) ON DELETE SET NULL`,
		`DELETE FROM schema_migrations WHERE version = 172`,
	} {
		_, err := db.DB.Exec(s)
		testutil.MustNoErr(t, err, s)
	}
}

func TestMigrate_V172_FamiliesAndThePool(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	k := testdb.MarkedGroup(t, db, "V172-K")
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: *k, EnsureRow: true}, "test"), "the kanban")
	ageFamilySchema(t, db)
	typ := func(code string, w, l float64) int64 {
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, width_in, length_in) VALUES ($1,$2,$3) RETURNING id`, code, w, l).Scan(&id), "type")
		return id
	}
	small, big, wide, typo := typ("V172-SMALL", 45, 48), typ("V172-BIG", 48, 45), typ("V172-WIDE", 32, 30), typ("V172-TYPO", 45.5, 48)
	half := typ("V172-HALF", 45, 0)
	collides := typ("45x48", 0, 0)
	var marker int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, bare_of) VALUES ('V172-SMALL-BARE', $1) RETURNING id`, small).Scan(&marker), "marker")
	const open, closed = "00000000-0000-0000-0000-000000000721", "00000000-0000-0000-0000-000000000722"
	for id, shut := range map[string]string{open: "NULL", closed: "NOW()"} {
		_, err := db.DB.Exec(`INSERT INTO demand_origins (origin_id, episode_key, kind, opened_at, closed_at)
			VALUES ($1, 'mnt|V172-K|V172-SMALL', 'kanban', NOW(), `+shut+`)`, id)
		testutil.MustNoErr(t, err, "episode")
	}

	familyOf := func(id int64) string {
		t.Helper()
		var code string
		testutil.MustNoErr(t, db.QueryRow(`SELECT f.code FROM bin_types bt JOIN carrier_families f ON f.id = bt.family_id WHERE bt.id = $1`, id).Scan(&code), "family")
		return code
	}
	for pass := 1; pass <= 2; pass++ {
		testutil.MustNoErr(t, db.MigrateForTest(), "migrate")
		for id, want := range map[int64]string{small: "45x48", big: "45x48", marker: "45x48", wide: "30x32", typo: "45.5x48", half: "V172-HALF"} {
			if got := familyOf(id); got != want {
				t.Errorf("pass %d: type %d is of %s, want %s", pass, id, got, want)
			}
		}
		if got := familyOf(collides); got == "45x48" {
			t.Errorf("pass %d: a type named as a footprint's family joined it by its code alone", pass)
		}
		var key string
		testutil.MustNoErr(t, db.QueryRow(`SELECT episode_key FROM demand_origins WHERE origin_id = $1`, open).Scan(&key), "key")
		if want := "mnt|" + strconv.FormatInt(*k, 10) + "|" + strconv.FormatInt(small, 10); key != want {
			t.Errorf("pass %d: the open episode's key is %s, want %s", pass, key, want)
		}
		testutil.MustNoErr(t, db.QueryRow(`SELECT episode_key FROM demand_origins WHERE origin_id = $1`, closed).Scan(&key), "key")
		if key != "mnt|V172-K|V172-SMALL" {
			t.Errorf("pass %d: a closed episode's key moved: %s", pass, key)
		}
	}
	if failing := db.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
		t.Errorf("migrations failing their post-condition: %v", failing)
	}
	// A type inserted with none stated gets a family of one, in the insert.
	if got := familyOf(typ("V172-NEW", 45, 48)); got != "V172-NEW" {
		t.Errorf("a new type is of %s, want a family of one", got)
	}
	// The census reads, and lists the typo beside its neighbour.
	rows, err := db.DB.Query(store.FamilyCensusSQL)
	testutil.MustNoErr(t, err, "the census")
	defer rows.Close()
	near := false
	for rows.Next() {
		var list, what, detail string
		testutil.MustNoErr(t, rows.Scan(&list, &what, &detail), "scan")
		near = near || list == "family_near_footprint"
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	if !near {
		t.Error("the census did not list two footprints under an inch apart")
	}
}
