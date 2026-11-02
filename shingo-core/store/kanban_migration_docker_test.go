//go:build docker

package store_test

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/schema"
)

// kanban_migration_docker_test.go — v169 against the shape it migrates.

// ageKanbanSchema takes a migrated database back to the shape v169 found: the
// three tables under their old names, hanging off nodes, and no kanbans. It is
// also v169's ROLLBACK as DDL, less re-inserting the properties.
func ageKanbanSchema(t *testing.T, db *store.DB) {
	t.Helper()
	for _, s := range []string{
		`ALTER TABLE kanban_levels DROP CONSTRAINT kanban_levels_kanban_node_id_fkey`,
		`ALTER TABLE kanban_supports DROP CONSTRAINT kanban_supports_kanban_node_id_fkey`,
		`ALTER TABLE kanban_layer_levels DROP CONSTRAINT kanban_layer_levels_kanban_node_id_fkey`,
		`DROP TABLE kanban_families`,
		`DROP TABLE kanban_sources`,
		`DROP TABLE kanbans`,
		`DROP INDEX idx_nodes_parent`,
		`DROP INDEX idx_node_properties_key`,
		`ALTER TABLE kanban_levels RENAME TO node_maintain_levels`,
		`ALTER TABLE kanban_supports RENAME TO node_maintain_supports`,
		`ALTER TABLE kanban_layer_levels RENAME TO node_maintain_layer_levels`,
		`ALTER TABLE node_maintain_levels RENAME COLUMN kanban_node_id TO group_node_id`,
		`ALTER TABLE node_maintain_supports RENAME COLUMN kanban_node_id TO group_node_id`,
		`ALTER TABLE node_maintain_layer_levels RENAME COLUMN kanban_node_id TO group_node_id`,
		`ALTER TABLE node_maintain_levels RENAME CONSTRAINT kanban_levels_pkey TO node_maintain_levels_pkey`,
		`ALTER TABLE node_maintain_levels RENAME CONSTRAINT kanban_levels_want_check TO node_maintain_levels_want_check`,
		`ALTER TABLE node_maintain_levels RENAME CONSTRAINT kanban_levels_bin_type_id_fkey TO node_maintain_levels_bin_type_id_fkey`,
		`ALTER TABLE node_maintain_supports RENAME CONSTRAINT kanban_supports_pkey TO node_maintain_supports_pkey`,
		`ALTER TABLE node_maintain_supports RENAME CONSTRAINT kanban_supports_process_node_id_fkey TO node_maintain_supports_process_node_id_fkey`,
		`ALTER TABLE node_maintain_layer_levels RENAME CONSTRAINT kanban_layer_levels_pkey TO node_maintain_layer_levels_pkey`,
		`ALTER TABLE node_maintain_layer_levels RENAME CONSTRAINT kanban_layer_levels_layers_check TO node_maintain_layer_levels_layers_check`,
		`ALTER TABLE node_maintain_layer_levels RENAME CONSTRAINT kanban_layer_levels_want_check TO node_maintain_layer_levels_want_check`,
		`ALTER TABLE node_maintain_layer_levels RENAME CONSTRAINT kanban_layer_levels_bin_type_id_fkey TO node_maintain_layer_levels_bin_type_id_fkey`,
		`ALTER TABLE node_maintain_levels ADD CONSTRAINT node_maintain_levels_group_node_id_fkey
			FOREIGN KEY (group_node_id) REFERENCES nodes(id) ON DELETE CASCADE`,
		`ALTER TABLE node_maintain_supports ADD CONSTRAINT node_maintain_supports_group_node_id_fkey
			FOREIGN KEY (group_node_id) REFERENCES nodes(id) ON DELETE CASCADE`,
		`ALTER TABLE node_maintain_layer_levels ADD CONSTRAINT node_maintain_layer_levels_group_node_id_fkey
			FOREIGN KEY (group_node_id) REFERENCES nodes(id) ON DELETE CASCADE`,
		`DELETE FROM schema_migrations WHERE version IN (169, 172)`,
	} {
		if _, err := db.DB.Exec(s); err != nil {
			t.Fatalf("age the schema: %s: %v", s, err)
		}
	}
}

// agedGroup is a group of one position, with what a plant had on its old shape.
type agedGroup struct {
	levels, layers, supports bool
	props                    map[string]string
}

// TestMigrate_Kanbans_ThePivot seeds the aged shape and checks each property's
// column, the census, and that a second pass changes nothing.
func TestMigrate_Kanbans_ThePivot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	ageKanbanSchema(t, db)

	var bt int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('AGED-T') RETURNING id`).Scan(&bt), "bin type")
	pos := &nodes.Node{Name: "AGED-PRESS", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(pos), "supported position")
	testdb.MarkedGroup(t, db, "AGED-SPILL")
	parent := testdb.MarkedGroup(t, db, "AGED-HALL")
	dotted := &nodes.Node{Name: "AGED-DOTTED", Enabled: true, ParentID: parent}
	testutil.MustNoErr(t, db.CreateNode(dotted), "a group under a parent")

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	groups := map[string]agedGroup{
		"AGED-FULL": {levels: true, supports: true, props: map[string]string{"maintain_enabled": "on", "strict_sourcing": "on",
			"maintenance_station": "edge.1", "overflow_destination": "AGED-SPILL"}},
		"AGED-DOTNAME":  {levels: true, props: map[string]string{"overflow_destination": "AGED-HALL.AGED-DOTTED", "maintain_enabled": "on"}},
		"AGED-FLAGOFF":  {levels: true, props: map[string]string{"maintain_enabled": "off", "maintenance_station": "edge.2"}},
		"AGED-DANGLING": {levels: true, props: map[string]string{"maintain_enabled": "on", "overflow_destination": "AGED-GONE"}},
		"AGED-SELF":     {levels: true, props: map[string]string{"maintain_enabled": "on", "overflow_destination": "AGED-SELF"}},
		"AGED-SUPPORTS": {supports: true, props: map[string]string{"maintenance_station": "edge.3", "overflow_destination": "AGED-SPILL"}},
		"AGED-LAYERS":   {layers: true},
		"AGED-STRICT":   {props: map[string]string{"strict_sourcing": "on"}},
		"AGED-PLAIN": {props: map[string]string{"maintain_enabled": "off", "strict_sourcing": "off",
			"maintenance_station": "edge.4", "overflow_destination": "AGED-SPILL"}},
		"AGED-PLAIN-BAD": {props: map[string]string{"overflow_destination": "AGED-NOWHERE"}},
	}
	ids := map[string]int64{}
	for name, g := range groups {
		id := testdb.MarkedGroup(t, db, name)
		ids[name] = *id
		if g.levels {
			exec(`INSERT INTO node_maintain_levels (group_node_id, bin_type_id, want) VALUES ($1,$2,3)`, *id, bt)
		}
		if g.layers {
			exec(`INSERT INTO node_maintain_layer_levels (group_node_id, bin_type_id, layers, want) VALUES ($1,$2,0,1)`, *id, bt)
		}
		if g.supports {
			exec(`INSERT INTO node_maintain_supports (group_node_id, process_node_id) VALUES ($1,$2)`, *id, pos.ID)
		}
		for k, v := range g.props {
			exec(`INSERT INTO node_properties (node_id, key, value) VALUES ($1,$2,$3)`, *id, k, v)
		}
	}

	// The census, read before the migration, as the lead reads a plant.
	census := readKanbanCensus(t, db)
	for _, want := range []string{
		"kanban_no_supports|AGED-DOTNAME",
		"kanban_no_supports|AGED-FLAGOFF",
		"layers_no_levels|AGED-LAYERS",
		"levels_flag_off|AGED-FLAGOFF",
		"levels_no_station|AGED-DANGLING",
		"levels_no_station|AGED-SELF",
		"overflow_unresolved|AGED-DANGLING",
		"overflow_unresolved|AGED-SELF",
		"plain_overflow_kept|AGED-PLAIN",
		"plain_overflow_kept|AGED-PLAIN-BAD",
		"plain_station_unread|AGED-PLAIN",
		"strict_no_levels|AGED-STRICT",
		"supports_no_levels|AGED-SUPPORTS",
	} {
		if !census[want] {
			t.Errorf("census is missing %s; it says %v", want, census)
		}
	}
	for _, not := range []string{"levels_no_station|AGED-FULL", "levels_flag_off|AGED-FULL", "kanban_no_supports|AGED-FULL",
		"overflow_unresolved|AGED-FULL", "overflow_unresolved|AGED-DOTNAME", "overflow_unresolved|AGED-PLAIN-BAD"} {
		if census[not] {
			t.Errorf("census lists %s, which it should not", not)
		}
	}

	testutil.MustNoErr(t, db.MigrateForTest(), "migrate")
	check := func() {
		t.Helper()
		kanban := func(name string) *store.Kanban {
			t.Helper()
			k, err := db.GetKanban(ids[name])
			testutil.MustNoErr(t, err, "read kanban "+name)
			return k
		}
		if k := kanban("AGED-FULL"); k == nil || k.StationID != "edge.1" || !k.Strict || !k.HasSupports {
			t.Errorf("AGED-FULL = %+v, want the three properties as columns", k)
		}
		if k := kanban("AGED-DOTNAME"); k == nil {
			t.Errorf("AGED-DOTNAME has no row")
		}
		if k := kanban("AGED-FLAGOFF"); k == nil || k.StationID != "edge.2" {
			t.Errorf("AGED-FLAGOFF = %+v, want a kanban with its station: a group with levels is one", k)
		}
		if k := kanban("AGED-DANGLING"); k == nil || k.StationID != "" {
			t.Errorf("AGED-DANGLING = %+v, want a NULL station", k)
		}
		if k := kanban("AGED-SELF"); k == nil {
			t.Error("AGED-SELF has no row")
		}
		if k := kanban("AGED-SUPPORTS"); k == nil || !k.HasSupports || k.StationID != "edge.3" {
			t.Errorf("AGED-SUPPORTS = %+v, want a row carrying its settings for the foreign key", k)
		}
		if k := kanban("AGED-LAYERS"); k == nil {
			t.Error("AGED-LAYERS has no row for its layer wants' foreign key")
		}
		if k := kanban("AGED-STRICT"); k == nil || !k.Strict {
			t.Errorf("AGED-STRICT = %+v, want a row so the fence stays up", k)
		}
		for _, plain := range []string{"AGED-PLAIN", "AGED-PLAIN-BAD"} {
			if k := kanban(plain); k != nil {
				t.Errorf("%s has a row %+v; a plain group has none", plain, k)
			}
		}
		if got := db.GetNodeProperty(ids["AGED-PLAIN"], nodes.PropOverflowDestination); got != "AGED-SPILL" {
			t.Errorf("AGED-PLAIN's overflow property = %q, want it untouched", got)
		}
		if got := db.GetNodeProperty(ids["AGED-PLAIN-BAD"], nodes.PropOverflowDestination); got != "AGED-NOWHERE" {
			t.Errorf("AGED-PLAIN-BAD's overflow property = %q, want it untouched", got)
		}
		var moved, flags int
		testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM node_properties
			WHERE key IN ('maintenance_station','strict_sourcing','overflow_destination')
			  AND node_id IN (SELECT node_id FROM kanbans)`).Scan(&moved), "moved keys")
		testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM node_properties WHERE key = 'maintain_enabled'`).Scan(&flags), "flag rows")
		if moved != 0 || flags != 0 {
			t.Errorf("%d moved keys and %d maintain_enabled rows remain; want none", moved, flags)
		}
		levels, err := db.ListKanbanLevels(ids["AGED-FULL"])
		testutil.MustNoErr(t, err, "levels")
		if len(levels) != 1 || levels[0].Want != 3 {
			t.Errorf("AGED-FULL's levels = %+v, want the row carried over", levels)
		}
	}
	check()

	// The second pass finds its work done, and the old names stay gone.
	testutil.MustNoErr(t, db.MigrateForTest(), "re-migrate")
	check()
	for _, old := range []string{"node_maintain_levels", "node_maintain_supports", "node_maintain_layer_levels"} {
		if !schema.TableAbsent(db.DB, old) {
			t.Errorf("%s exists after a second pass", old)
		}
	}
	if failing := db.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
		t.Errorf("migrations failing their post-condition: %v", failing)
	}
}

func readKanbanCensus(t *testing.T, db *store.DB) map[string]bool {
	t.Helper()
	rows, err := db.DB.Query(store.KanbanMigrationCensusSQL)
	testutil.MustNoErr(t, err, "census")
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var list, group, detail string
		testutil.MustNoErr(t, rows.Scan(&list, &group, &detail), "scan census")
		if detail == "" || strings.TrimSpace(group) == "" {
			t.Errorf("census row %s has no detail or group: %q %q", list, group, detail)
		}
		out[list+"|"+group] = true
	}
	testutil.MustNoErr(t, rows.Err(), "census rows")
	return out
}

// TestMigrate_V171_TheEpisodeKindIsKanban: rows under the old word, open and closed,
// read as kanban afterwards with their episode keys as they were, another kind's row is
// untouched, and a second pass changes nothing.
func TestMigrate_V171_TheEpisodeKindIsKanban(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	insert := func(id, key, kind, closed string) {
		t.Helper()
		_, err := db.DB.Exec(`INSERT INTO demand_origins (origin_id, episode_key, kind, opened_at, closed_at)
			VALUES ($1, $2, $3, NOW(), `+closed+`)`, id, key, kind)
		testutil.MustNoErr(t, err, "insert "+key)
	}
	const open, shut, other = "00000000-0000-0000-0000-000000000171", "00000000-0000-0000-0000-000000000172", "00000000-0000-0000-0000-000000000173"
	insert(open, "mnt|V171|T", "maintain", "NULL")
	insert(shut, "mnt|V171|T", "maintain", "NOW()")
	insert(other, "chg|V171|P", "changeover", "NULL")
	_, err := db.DB.Exec(`DELETE FROM schema_migrations WHERE version = 171`)
	testutil.MustNoErr(t, err, "forget v171")

	for pass := 1; pass <= 2; pass++ {
		testutil.MustNoErr(t, db.MigrateForTest(), "migrate")
		for id, want := range map[string][2]string{open: {"kanban", "mnt|V171|T"}, shut: {"kanban", "mnt|V171|T"},
			other: {"changeover", "chg|V171|P"}} {
			var kind, key string
			testutil.MustNoErr(t, db.DB.QueryRow(`SELECT kind, episode_key FROM demand_origins WHERE origin_id = $1`, id).Scan(&kind, &key), "read")
			if kind != want[0] || key != want[1] {
				t.Errorf("pass %d: %s = %s %s, want %v", pass, id, kind, key, want)
			}
		}
	}
	if failing := db.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
		t.Errorf("migrations failing their post-condition: %v", failing)
	}
}

// TestMigrate_V172_TheFamilies: types aged to before the family. One family per normalised
// footprint coded by it, either way round; a family of one per remaining type; a marker takes its
// carrier's; a code a footprint family already holds does not join it; an open kanban episode's
// key moves to ids, a closed one's does not; every later insert names a family or takes one; and a
// second pass changes nothing.
func TestMigrate_V172_TheFamilies(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	k := testdb.MarkedGroup(t, db, "V172-K")
	var small, big, wide, unsized, named, marker int64
	insert := func(to *int64, code string, w, l float64) {
		t.Helper()
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, width_in, length_in) VALUES ($1,$2,$3) RETURNING id`,
			code, w, l).Scan(to), "type "+code)
	}
	insert(&small, "V172-SMALL", 45, 48)
	insert(&big, "V172-BIG", 48, 45)
	insert(&wide, "V172-WIDE", 32.5, 30)
	insert(&unsized, "V172-UNSIZED", 0, 0)
	insert(&named, "45x48", 0, 0)
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, bare_of) VALUES ('V172-SMALL-BARE', $1) RETURNING id`, small).Scan(&marker), "marker")
	const open, shut = "00000000-0000-0000-0000-000000000173", "00000000-0000-0000-0000-000000000174"
	exec(`INSERT INTO demand_origins (origin_id, episode_key, kind, opened_at) VALUES ($1, 'mnt|V172-K|V172-SMALL', 'kanban', NOW())`, open)
	exec(`INSERT INTO demand_origins (origin_id, episode_key, kind, opened_at, closed_at) VALUES ($1, 'mnt|V172-K|V172-BIG', 'kanban', NOW(), NOW())`, shut)

	exec(`DROP TRIGGER bin_types_family_of_one ON bin_types`)
	exec(`ALTER TABLE bin_types DROP COLUMN family_id`)
	exec(`DROP TABLE kanban_families`)
	exec(`DELETE FROM carrier_families`)
	exec(`DELETE FROM schema_migrations WHERE version = 172`)

	check := func(pass int) {
		t.Helper()
		family := func(id int64) string {
			t.Helper()
			var code string
			testutil.MustNoErr(t, db.QueryRow(`SELECT f.code FROM bin_types bt JOIN carrier_families f ON f.id = bt.family_id WHERE bt.id = $1`, id).Scan(&code), "family")
			return code
		}
		for id, want := range map[int64]string{small: "45x48", big: "45x48", wide: "30x32.5", unsized: "V172-UNSIZED", marker: "45x48"} {
			if got := family(id); got != want {
				t.Errorf("pass %d: type %d is of %q, want %q", pass, id, got, want)
			}
		}
		if got := family(named); got == "45x48" {
			t.Errorf("pass %d: an unsized type coded 45x48 joined the footprint's family", pass)
		}
		var key string
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT episode_key FROM demand_origins WHERE origin_id = $1`, open).Scan(&key), "open key")
		if want := protocol.KanbanEpisodeKey(*k, small); key != want {
			t.Errorf("pass %d: the open episode's key = %q, want %q", pass, key, want)
		}
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT episode_key FROM demand_origins WHERE origin_id = $1`, shut).Scan(&key), "shut key")
		if key != "mnt|V172-K|V172-BIG" {
			t.Errorf("pass %d: the closed episode's key = %q, want it as it was", pass, key)
		}
	}
	testutil.MustNoErr(t, db.MigrateForTest(), "migrate")
	check(1)
	testutil.MustNoErr(t, db.MigrateForTest(), "re-migrate")
	check(2)
	if failing := db.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
		t.Errorf("migrations failing their post-condition: %v", failing)
	}

	var late int64
	insert(&late, "V172-LATE", 45, 48)
	var code string
	testutil.MustNoErr(t, db.QueryRow(`SELECT f.code FROM bin_types bt JOIN carrier_families f ON f.id = bt.family_id WHERE bt.id = $1`, late).Scan(&code), "late")
	if code != "V172-LATE" {
		t.Errorf("a type inserted after the migration is of %q, want a family of its own", code)
	}
	var census int
	rows, err := db.DB.Query(store.FamilyCensusSQL)
	testutil.MustNoErr(t, err, "the census runs")
	defer rows.Close()
	for rows.Next() {
		census++
	}
	testutil.MustNoErr(t, rows.Err(), "census rows")
}
