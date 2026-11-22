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
