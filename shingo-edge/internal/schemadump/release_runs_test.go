package schemadump_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shingoedge/internal/schemadump"
	"shingoedge/store"
	"shingoedge/store/schema"
)

// v59File is a file in a plant's v59 schema (store/testdata), recorded at
// v1..v59, with one synthetic order, process, style and claim to read.
func v59File(t *testing.T, file string) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join(moduleRoot(t), "store", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plant.db")
	stmts := []string{string(ddl)}
	for v := 1; v <= store.RetiredChain; v++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO schema_migrations (version) VALUES (%d)`, v))
	}
	plant(t, path, append(stmts,
		`INSERT INTO processes (id, name) VALUES (1, 'RUNS-PROC')`,
		`INSERT INTO styles (id, process_id, name) VALUES (1, 1, 'RUNS-STYLE')`,
		`INSERT INTO style_node_claims (style_id, core_node_name, swap_mode) VALUES (1, 'RUNS-NODE', 'two_robot')`,
		`INSERT INTO orders (uuid, order_type, status) VALUES ('RUNS-1', 'retrieve', 'queued')`)...)
	return path
}

func countOf(t *testing.T, path, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestRetiredChainReleaseRunsOnThisSchema is v60's drop rule and its way back:
// on each plant shape this build has migrated, the previous release boots and
// reads orders, processes and claims, and this build then starts again on what
// that boot added back, with the shape of a born database.
func TestRetiredChainReleaseRunsOnThisSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rel := retiredRelease(ctx, t)
	born := schema.ParseShape(buildFresh(t))
	for _, file := range []string{"plant_a_v59.sql", "plant_b_v59.sql"} {
		t.Run(file, func(t *testing.T) {
			path := v59File(t, file)
			if err := startThisBuild(path); err != nil {
				t.Fatalf("this build on the v59 shape: %v", err)
			}
			if _, err := rel.Boot(ctx, path); err != nil {
				t.Fatal(err)
			}
			if err := startThisBuild(path); err != nil {
				t.Fatalf("this build after %s: %v", rel.Rev, err)
			}
			dump, err := schemadump.Dump(path)
			if err != nil {
				t.Fatal(err)
			}
			if missing, differs := schema.Compare(born, schema.ParseShape(dump)); len(missing)+len(differs) > 0 {
				t.Errorf("down and up again is not a born database's shape: missing %q, differs %q", missing, differs)
			}
		})
	}
}

// TestAHandDroppedTableBelowHead is what a refusal cannot cover: a v59 file
// with a table dropped by hand gets v60, then fails by name. No row outside
// the drop set is lost, and the previous release still opens the file and
// makes the table again, after which this build starts.
func TestAHandDroppedTableBelowHead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rel := retiredRelease(ctx, t)
	path := v59File(t, "plant_b_v59.sql")
	plant(t, path, `DROP TABLE shifts`)

	err := startThisBuild(path)
	if err == nil || !strings.Contains(err.Error(), "table shifts") {
		t.Fatalf("start = %v, want a failure naming the table", err)
	}
	if countOf(t, path, `SELECT COUNT(*) FROM schema_migrations WHERE version = 60`) != 1 {
		t.Error("v60 is not recorded: the pin describes another path")
	}
	for _, table := range []string{"orders", "processes", "styles", "style_node_claims"} {
		if countOf(t, path, `SELECT COUNT(*) FROM `+table) != 1 {
			t.Errorf("%s lost its row", table)
		}
	}
	if _, err := rel.Boot(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := startThisBuild(path); err != nil {
		t.Errorf("this build after %s made the table again: %v", rel.Rev, err)
	}
}

// mainsV60Release is the release main shipped the Edge's v60 in, the last one
// before the cancel column: its boot path makes a file at v60 the way main's
// chain does.
const mainsV60Release = "v0.20.1"

// TestV61_AFileAtMainsV60ReachesV61WithItsRows pins the Edge's numbers after
// main was merged into the branch that wrote the cancel column (brief I, I2):
// main's v60 stays v60, and orders.cancel_intent, v60 in the chain that branch
// carried, is v61 above it.
//
//   - main's: a plant's v59 file started by mainsV60Release is at v60 with no
//     cancel column; given rows at v60 (in orders, the table both migrations
//     touch, and in two of the tables v60 rebuilds), this build starts on it at
//     v61, keeps every row of every table over its v60 columns, reads no cancel
//     on any order, and leaves a born database's shape. A born
//     database is at v61 too.
//   - the branch's: a v59 file with the column and 60 recorded, as that
//     branch's build left it, gets main's v60 again (its Verify sees the
//     columns v60 drops) and v61, keeps the order's cancel, and leaves a born
//     database's shape.
func TestV61_AFileAtMainsV60ReachesV61WithItsRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rel, err := schemadump.BuildRelease(ctx, moduleRoot(t), mainsV60Release, releaseWork)
	if err != nil {
		t.Fatalf("build %s: %v", mainsV60Release, err)
	}
	const head = `SELECT MAX(version) FROM schema_migrations`
	const hasCancel = `SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name = 'cancel_intent'`
	fresh, err := schemadump.BuildFresh(t.TempDir())
	if err != nil {
		t.Fatalf("a born database: %v", err)
	}
	if got := countOf(t, fresh, head); got != 61 {
		t.Errorf("a born database is at v%d, want v61", got)
	}
	bornDump, err := schemadump.Dump(fresh)
	if err != nil {
		t.Fatal(err)
	}
	born := schema.ParseShape(bornDump)
	sameShape := func(t *testing.T, path string) {
		t.Helper()
		dump, err := schemadump.Dump(path)
		if err != nil {
			t.Fatal(err)
		}
		if missing, differs := schema.Compare(born, schema.ParseShape(dump)); len(missing)+len(differs) > 0 {
			t.Errorf("not a born database's shape: missing %q, differs %q", missing, differs)
		}
	}

	for _, file := range []string{"plant_a_v59.sql", "plant_b_v59.sql"} {
		t.Run("main's v60, "+file, func(t *testing.T) {
			path := v59File(t, file)
			if _, err := rel.Boot(ctx, path); err != nil {
				t.Fatal(err)
			}
			if countOf(t, path, head) != 60 || countOf(t, path, hasCancel) != 0 {
				t.Fatalf("%s did not leave its v60: head v%d, cancel column %d",
					rel.Rev, countOf(t, path, head), countOf(t, path, hasCancel))
			}
			plant(t, path,
				`INSERT INTO orders (uuid, order_type, status, delivery_node, source_node, quantity, steps_json,
					release_intent, release_held, core_version, want, station_wait, wait_kind)
					VALUES ('V61-2', 'complex', 'staged', 'V61-LINE', 'V61-HOME', 2, '[{"action":"pickup"}]',
					'{"station_wait":1}', 'held for the curtain', 7, 'low', 1, 'release')`,
				`INSERT INTO orders (uuid, order_type, status, sibling_order_id, core_version, staged_at)
					VALUES ('V61-3', 'retrieve', 'confirmed', 1, 3, '2026-11-25 18:00:00')`,
				`INSERT INTO operator_stations (process_id, code, name, sequence, core_loader_key)
					VALUES (1, 'V61-OP', 'V61 screen', 2, 'V61-LOADER')`,
				`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by)
					VALUES (1, NULL, 1, 'running', 'V61-CALLER')`)
			cols := columnsOf(t, path)
			before := rowsOf(t, path, cols)
			if err := startThisBuild(path); err != nil {
				t.Fatalf("this build on %s's v60: %v", rel.Rev, err)
			}
			if got := countOf(t, path, head); got != 61 {
				t.Errorf("head v%d after the start, want v61", got)
			}
			after := rowsOf(t, path, cols)
			for table, rows := range before {
				if after[table] != rows {
					t.Errorf("the start changed %s's rows:\nbefore:\n%s\nafter:\n%s", table, rows, after[table])
				}
			}
			if n := countOf(t, path, `SELECT COUNT(*) FROM orders WHERE cancel_intent = ''`); n != 3 {
				t.Errorf("%d orders read '' as their cancel, want all 3", n)
			}
			sameShape(t, path)
		})
	}

	t.Run("the branch's v60", func(t *testing.T) {
		path := v59File(t, "plant_b_v59.sql")
		plant(t, path,
			`ALTER TABLE orders ADD COLUMN cancel_intent TEXT NOT NULL DEFAULT ''`,
			`INSERT INTO schema_migrations (version) VALUES (60)`,
			`UPDATE orders SET cancel_intent = '{"asked":"V61"}'`)
		if err := startThisBuild(path); err != nil {
			t.Fatalf("this build on the branch's v60: %v", err)
		}
		if got := countOf(t, path, `SELECT COUNT(*) FROM schema_migrations WHERE version IN (60, 61)`); got != 2 {
			t.Errorf("%d of v60 and v61 recorded, want both", got)
		}
		if n := countOf(t, path, `SELECT COUNT(*) FROM orders WHERE cancel_intent = '{"asked":"V61"}'`); n != 1 {
			t.Errorf("%d orders kept their cancel, want 1", n)
		}
		sameShape(t, path)
	})
}

// columnsOf is every table of path but schema_migrations, with its columns.
func columnsOf(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name <> 'schema_migrations' ORDER BY m.name, c.cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		out[table] = append(out[table], column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// rowsOf is every row of each table in cols over those columns, each value as
// SQL's quote() writes it, one line a row in rowid order.
func rowsOf(t *testing.T, path string, cols map[string][]string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := map[string]string{}
	for table, columns := range cols {
		quoted := make([]string, len(columns))
		for i, c := range columns {
			quoted[i] = `quote("` + c + `")`
		}
		rows, err := db.Query(`SELECT ` + strings.Join(quoted, ` || ',' || `) + ` FROM "` + table + `" ORDER BY rowid`)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		out[table] = strings.Join(lines, "\n")
	}
	return out
}
