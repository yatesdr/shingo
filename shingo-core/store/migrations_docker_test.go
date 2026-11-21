//go:build docker

package store_test

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/schema"
)

// rowsOf is a one-text-column query's rows, in order.
func rowsOf(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// tableComment is a table's COMMENT, "" when it has none.
func tableComment(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRow(`SELECT obj_description(('public.' || $1)::regclass, 'pg_class')`, table).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c.String
}

// TestV186_ReportRowsPerStationAndProcess: v186 keys plant_claims_reports by
// (station_uid, process_id), so two stations' rows for one process stand side
// by side. Down keeps one row per process, the one of the station the mirror
// names, else the newest, and puts the one-row-per-process key and v185's
// comment back; up again restores the key and the comment and keeps every row
// down kept; a re-run of up (its row deleted) changes nothing.
func TestV186_ReportRowsPerStationAndProcess(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	key := func() string { return schema.ConstraintDef(db, "plant_claims_reports", "plant_claims_reports_pkey") }
	const rowsQ = `SELECT process_id || ':' || station_uid || ':' || digest FROM plant_claims_reports ORDER BY process_id, station_uid`
	if got := key(); got != "PRIMARY KEY (station_uid, process_id)" {
		t.Fatalf("plant_claims_reports key at this build = %q, want the station and the process", got)
	}
	for _, q := range []string{
		// P1: two stations, the mirror names the older writer's; P2: one; P3:
		// two, no mirror row, so the newest is kept.
		`INSERT INTO plant_claims_reports (station_uid, process_id, digest, received_at) VALUES
		   ('stn-a', 'P1', 'a1', '2026-06-01T10:00:00Z'), ('stn-b', 'P1', 'b1', '2026-06-01T11:00:00Z'),
		   ('stn-a', 'P2', 'a2', '2026-06-01T10:00:00Z'),
		   ('stn-a', 'P3', 'a3', '2026-06-01T12:00:00Z'), ('stn-b', 'P3', 'b3', '2026-06-01T11:00:00Z')`,
		`INSERT INTO process_styles (process_id, style_id, station_uid) VALUES ('P1', 'S1', 'stn-a')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	upComment := tableComment(t, db.DB, "plant_claims_reports")

	if err := testdb.MigrateDownThrough(cfg, 186); err != nil {
		t.Fatalf("down through 186: %v", err)
	}
	if got := key(); got != "PRIMARY KEY (process_id)" {
		t.Errorf("key after down = %q, want v185's, the process alone", got)
	}
	kept := []string{"P1:stn-a:a1", "P2:stn-a:a2", "P3:stn-a:a3"}
	if got := rowsOf(t, db.DB, rowsQ); !reflect.DeepEqual(got, kept) {
		t.Errorf("rows after down = %v, want %v (the mirror's station, else the newest)", got, kept)
	}
	if got, want := tableComment(t, db.DB, "plant_claims_reports"), "Per process, the station whose plant-claims report the mirror holds and the digest it carried (v143); read back onto each station's heartbeat ack."; got != want {
		t.Errorf("comment after down = %q, want v185's %q", got, want)
	}

	for boot := 1; boot <= 2; boot++ {
		up, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("up again (boot %d): %v", boot, err)
		}
		up.Close()
		if got := key(); got != "PRIMARY KEY (station_uid, process_id)" {
			t.Errorf("boot %d: key = %q, want the station and the process", boot, got)
		}
		if got := rowsOf(t, db.DB, rowsQ); !reflect.DeepEqual(got, kept) {
			t.Errorf("boot %d: rows = %v, want %v", boot, got, kept)
		}
		if got := tableComment(t, db.DB, "plant_claims_reports"); got != upComment {
			t.Errorf("boot %d: comment = %q, want the one this build is born with, %q", boot, got, upComment)
		}
		// The second boot re-runs v186 on its own result.
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 186`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestV187_TheCatalogDigestColumn: v187 adds edge_cells.catalog_digest with
// an empty default; down drops it and keeps every cell; up again brings it back
// empty on every row (the next catalog an Edge sends writes it), and a re-run
// of up changes nothing.
func TestV187_TheCatalogDigestColumn(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	if err := db.UpsertEdgeCells("stn-a", []store.EdgeCell{{CellLabel: "PLC-1"}}, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := testdb.MigrateDownThrough(cfg, 187); err != nil {
		t.Fatalf("down through 187: %v", err)
	}
	if !schema.ColumnAbsent(db, "edge_cells", "catalog_digest") {
		t.Fatal("edge_cells.catalog_digest is there after down 187")
	}
	const cellsQ = `SELECT station_uid || ':' || cell_label || ':' || stale::text FROM edge_cells ORDER BY station_uid, cell_label`
	if got := rowsOf(t, db.DB, cellsQ); !reflect.DeepEqual(got, []string{"stn-a:PLC-1:false"}) {
		t.Errorf("cells after down = %v, want the one live cell", got)
	}
	for boot := 1; boot <= 2; boot++ {
		up, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("up again (boot %d): %v", boot, err)
		}
		up.Close()
		if got := rowsOf(t, db.DB, `SELECT cell_label || ':' || catalog_digest FROM edge_cells ORDER BY cell_label`); !reflect.DeepEqual(got, []string{"PLC-1:"}) {
			t.Errorf("boot %d: cells = %v, want the cell with no digest", boot, got)
		}
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 187`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestV188_NotesGoAndTheRegistryCommentsAreTrue: v188 drops
// cms_transactions.notes and corrects edge_registry's comments (an Edge mints
// its own uid; Core mints one only at enrollment). Down brings the column back
// with its default and v185's comments; up again drops it and applies the true
// text; a re-run of up changes nothing.
func TestV188_NotesGoAndTheRegistryCommentsAreTrue(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	stationUID := func() string {
		t.Helper()
		var c sql.NullString
		if err := db.QueryRow(`SELECT col_description('public.edge_registry'::regclass,
			(SELECT attnum FROM pg_attribute WHERE attrelid = 'public.edge_registry'::regclass AND attname = 'station_uid'))`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c.String
	}
	check := func(when string, notes bool, mints string) {
		t.Helper()
		if got := schema.ColumnExists(db, "cms_transactions", "notes"); got != notes {
			t.Errorf("%s: cms_transactions.notes there = %v, want %v", when, got, notes)
		}
		for what, c := range map[string]string{"table": tableComment(t, db.DB, "edge_registry"), "station_uid": stationUID()} {
			if !strings.Contains(c, mints) {
				t.Errorf("%s: edge_registry's %s comment does not say %q:\n%s", when, what, mints, c)
			}
		}
	}
	check("at this build", false, "by the Edge at its first boot")
	if err := testdb.MigrateDownThrough(cfg, 188); err != nil {
		t.Fatalf("down through 188: %v", err)
	}
	check("after down", true, "Core mints")
	if _, err := db.Exec(`WITH n AS (INSERT INTO nodes (name) VALUES ('V188-N') RETURNING id)
		INSERT INTO cms_transactions (node_id, cat_id, source_type) SELECT id, 'C1', 'movement' FROM n`); err != nil {
		t.Fatalf("v0.18.0's insert, which names no notes: %v", err)
	}
	for boot := 1; boot <= 2; boot++ {
		up, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("up again (boot %d): %v", boot, err)
		}
		up.Close()
		check(fmt.Sprintf("boot %d", boot), false, "by the Edge at its first boot")
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 188`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestV189_CellConfigGoesAndV185StillReRuns: v189 drops cell_config. v185
// renames two of its columns and comments one, so a re-run of v185 on a
// database past v189 (its row deleted, as the runner does on a failed verify)
// must neither fail nor loop: it boots, twice. Down recreates the table empty
// in v185's shape, its key, index and comment; up again drops it.
func TestV189_CellConfigGoesAndV185StillReRuns(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	if !schema.TableAbsent(db, "cell_config") {
		t.Fatal("cell_config is there at this build")
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 185`); err != nil {
		t.Fatal(err)
	}
	for boot := 1; boot <= 2; boot++ {
		up, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("boot %d after v185's row went (v185 re-runs past v189): %v", boot, err)
		}
		up.Close()
	}
	var applied int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = 185`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("v185 recorded %d times (err %v) after its re-run, want once", applied, err)
	}

	if err := testdb.MigrateDownThrough(cfg, 189); err != nil {
		t.Fatalf("down through 189: %v", err)
	}
	if got := schema.ConstraintDef(db, "cell_config", "cell_config_pkey"); got != "PRIMARY KEY (cell_label)" {
		t.Errorf("cell_config key after down = %q, want PRIMARY KEY (cell_label)", got)
	}
	if !schema.IndexExists(db, "cell_config_station_uid_idx") {
		t.Error("cell_config_station_uid_idx is missing after down")
	}
	var rows int
	var comment sql.NullString
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM cell_config), col_description('public.cell_config'::regclass, 1)`).Scan(&rows, &comment); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || !strings.Contains(comment.String, "(cell_id until v185)") {
		t.Errorf("after down: %d rows, cell_label comment %q; want an empty table with v185's comment", rows, comment.String)
	}
	up, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("up again: %v", err)
	}
	up.Close()
	if !schema.TableAbsent(db, "cell_config") {
		t.Error("cell_config is there after up again")
	}
}
