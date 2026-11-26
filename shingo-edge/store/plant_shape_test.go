package store

import (
	"database/sql"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"shingo/protocol/testutil"
)

// The two plants' Edge schemas at v59, as sqlite_master holds them
// (testdata/plant_*_v59.sql): shingo's own tables, no rows. A migration that
// assumes a plant's file has a born database's shape is wrong on that Pi, so
// the shapes are the plants' catalogues and not typed from a list of findings.

// plantShapeDB is a database in a plant's schema, recorded at v1..v59, with two
// synthetic rows in every table (one where a CHECK holds it to one) and each
// AUTOINCREMENT sequence left five above its rows, as deleted rows leave it.
func plantShapeDB(t *testing.T, file string) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("testdata", file))
	testutil.MustNoErr(t, err, "read the plant's schema")
	path := filepath.Join(t.TempDir(), "plant.db")
	db := rawDB(t, path)
	execAll(t, db, string(ddl))
	for v := 1; v <= 59; v++ {
		execAll(t, db, fmt.Sprintf(`INSERT INTO schema_migrations (version) VALUES (%d)`, v))
	}
	// The columns a CHECK holds to a vocabulary.
	checked := map[string]string{
		"node_lineside_bucket.state":   "'active'",
		"process_routing_nodes.role":   "'source'",
		"process_routing_nodes.origin": "'engineer'",
	}
	oneRow := map[string]bool{"production_tick_cursor": true, "scene_geometry_meta": true}
	for _, table := range queryLines(t, db, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations' ORDER BY name`) {
		var names, kinds []string
		for _, col := range queryLines(t, db, `SELECT name, upper(type) FROM pragma_table_info(?)`, table) {
			name, kind, _ := strings.Cut(col, " ")
			names = append(names, `"`+name+`"`)
			kinds = append(kinds, kind)
		}
		for n := 1; n <= 2; n++ {
			if n > 1 && oneRow[table] {
				break
			}
			vals := make([]string, len(names))
			for i, kind := range kinds {
				vals[i] = fmt.Sprintf("'syn-%d'", n)
				if strings.Contains(kind, "INT") || kind == "REAL" {
					vals[i] = fmt.Sprint(n)
				}
				if v, ok := checked[table+"."+strings.Trim(names[i], `"`)]; ok {
					vals[i] = v
				}
			}
			execAll(t, db, `INSERT INTO "`+table+`" (`+strings.Join(names, ", ")+`) VALUES (`+strings.Join(vals, ", ")+`)`)
		}
	}
	execAll(t, db, `UPDATE sqlite_sequence SET seq = seq + 5`)
	testutil.MustNoErr(t, db.Close(), "close")
	return path
}

// shapeOf is a database's shape as SQLite's pragmas report it, one line per
// fact: tables, columns, foreign keys, indexes, triggers. Of the stored text it
// reads only a partial index's predicate, so two spellings of one shape are one
// shape.
func shapeOf(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, q := range []string{
		`SELECT m.type, m.name FROM sqlite_master m WHERE m.type IN ('table', 'view', 'trigger') AND m.name NOT LIKE 'sqlite_%'`,
		`SELECT 'column', m.name || '.' || c.name, upper(c.type) || CASE WHEN c."notnull" THEN ' not null' ELSE '' END
			|| COALESCE(' default ' || c.dflt_value, '') || CASE WHEN c.pk THEN ' key ' || c.pk ELSE '' END
			FROM sqlite_master m, pragma_table_info(m.name) c WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'`,
		`SELECT 'key', m.name || '.' || f."from", '-> ' || f."table", 'on delete ' || f.on_delete
			FROM sqlite_master m, pragma_foreign_key_list(m.name) f WHERE m.type = 'table'`,
		`SELECT 'index', m.name || '.' || CASE i.origin WHEN 'c' THEN i.name ELSE i.origin END,
			(SELECT group_concat(COALESCE(x.name, '(expr)') || CASE WHEN x."desc" THEN ' desc' ELSE '' END)
				FROM pragma_index_xinfo(i.name) x WHERE x.key),
			CASE WHEN i."unique" THEN 'unique' ELSE '' END,
			COALESCE((SELECT substr(s.sql, instr(upper(s.sql), 'WHERE')) FROM sqlite_master s WHERE s.name = i.name AND i.partial), '')
			FROM sqlite_master m, pragma_index_list(m.name) i WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'`,
	} {
		for _, line := range queryLines(t, db, q) {
			out[strings.Join(strings.Fields(line), " ")] = true
		}
	}
	return out
}

// shapeAgainst is what got has that want does not (+) and lacks (-), sorted.
// An extra table is one line, not one per column, key and index it carries.
func shapeAgainst(got, want map[string]bool) []string {
	var out []string
	for line := range got {
		if want[line] {
			continue
		}
		kind, rest, _ := strings.Cut(line, " ")
		if table, _, ok := strings.Cut(rest, "."); ok && kind != "table" && !want["table "+table] {
			continue
		}
		out = append(out, "+ "+line)
	}
	for line := range want {
		if !got[line] {
			out = append(out, "- "+line)
		}
	}
	slices.Sort(out)
	return out
}

// keptColumns is every table the born database has, with those of its columns
// the file at path has too: the columns a start keeps of a plant's file. A
// column a migration above v60 adds (v61's orders.cancel_intent) is not one of
// them, so it is read neither before the start nor after it.
func keptColumns(t *testing.T, path, born string) map[string][]string {
	t.Helper()
	db, shape := rawDB(t, path), rawDB(t, born)
	out := map[string][]string{}
	tables := queryLines(t, shape, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations' ORDER BY name`)
	for _, table := range tables {
		has := queryLines(t, db, `SELECT name FROM pragma_table_info(?)`, table)
		for _, col := range queryLines(t, shape, `SELECT name FROM pragma_table_info(?)`, table) {
			if slices.Contains(has, col) {
				out[table] = append(out[table], col)
			}
		}
	}
	testutil.MustNoErr(t, db.Close(), "close")
	testutil.MustNoErr(t, shape.Close(), "close")
	return out
}

// keptRows is every row of every table in cols, over its columns there, and
// those tables' AUTOINCREMENT sequences: what v60 keeps of a plant's file.
func keptRows(t *testing.T, path string, cols map[string][]string) map[string]string {
	t.Helper()
	db := rawDB(t, path)
	out := map[string]string{}
	for table, columns := range cols {
		lines := queryLines(t, db, `SELECT "`+strings.Join(columns, `", "`)+`" FROM "`+table+`"`)
		slices.Sort(lines)
		out[table] = strings.Join(lines, "\n")
		if seq := queryLines(t, db, `SELECT seq FROM sqlite_sequence WHERE name = ?`, table); len(seq) > 0 {
			out["sequence of "+table] = strings.Join(seq, ",")
		}
	}
	testutil.MustNoErr(t, db.Close(), "close")
	return out
}

// TestPlantShapes_WhatAStartLeaves starts an Edge on each plant's shape: every
// kept column's rows and every sequence are kept, and the shape is a born
// database's, by the pragmas (shapeOf).
func TestPlantShapes_WhatAStartLeaves(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"plant_a_v59.sql", "plant_b_v59.sql"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			path, born := plantShapeDB(t, file), bornDB(t)
			cols := keptColumns(t, path, born)
			before := keptRows(t, path, cols)
			db, err := Open(path)
			testutil.MustNoErr(t, err, "start")
			if drift := db.DriftAtBoot(); len(drift) > 0 {
				t.Errorf("the start reports drift:\n  %s", strings.Join(drift, "\n  "))
			}
			testutil.MustNoErr(t, db.Close(), "close")
			after := keptRows(t, path, cols)
			if len(after) != len(before) || len(before) < 60 {
				t.Errorf("%d tables and sequences after a start, %d before", len(after), len(before))
			}
			for _, key := range slices.Sorted(maps.Keys(before)) {
				if before[key] != after[key] {
					t.Errorf("a start changed %s:\nbefore:\n%s\nafter:\n%s", key, before[key], after[key])
				}
			}
			if got := shapeAgainst(shapeOf(t, rawDB(t, path)), shapeOf(t, rawDB(t, born))); len(got) > 0 {
				t.Errorf("against a born database:\n  %s", strings.Join(got, "\n  "))
			}
		})
	}
}
