//go:build docker

package store_test

import (
	"database/sql"
	"reflect"
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
