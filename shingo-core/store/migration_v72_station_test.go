//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// seedOldStation puts the pre-rename value into all four tables that store one.
//
// It runs AFTER migrations, which is the only way to get "before" data in front
// of a migration that has already been applied. Re-opening the database below
// then finds the old rows, fails v72's post-condition, and re-runs it — the
// self-heal path, which is exactly the path a plant takes.
func seedOldStation(t *testing.T, db *store.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity)
		 VALUES ('v72-old-station', 'core-spot', 'move', 'pending', 1)`,
		`INSERT INTO mission_telemetry (order_id, station_id) VALUES (987654, 'core-spot')`,
		`INSERT INTO cell_config (cell_id, station, primary_process_id)
		 VALUES ('v72-cell', 'core-spot', 1)`,
		`INSERT INTO dashboards (name, kind, stations_json)
		 VALUES ('v72-board', 'task-board', '["plant-a.line-1","core-spot","zzz"]')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

func countOldStation(t *testing.T, db *store.DB) (orders, telem, cells, boards int) {
	t.Helper()
	q := func(sql string) int {
		var n int
		if err := db.QueryRow(sql).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", sql, err)
		}
		return n
	}
	return q(`SELECT count(*) FROM orders WHERE station_id = 'core-spot'`),
		q(`SELECT count(*) FROM mission_telemetry WHERE station_id = 'core-spot'`),
		q(`SELECT count(*) FROM cell_config WHERE station = 'core-spot'`),
		q(`SELECT count(*) FROM dashboards WHERE stations_json LIKE '%"core-spot"%'`)
}

// TestV72_RenamesTheOperatorStationEverywhere is the whole reason this rename
// is a migration and not a change to two string literals.
//
// Leaving old rows behind does not break anything loudly. It splits things: the
// orphan summary GROUPs BY the station id, so one station becomes two rows and
// every count with it; a saved dashboard scoped to the old name matches nothing
// and goes blank; and a heartbeat board cross-filtered against that scope
// empties with it. Silence nobody notices is the failure this campaign exists
// to stop, so all four move together or none do.
func TestV72_RenamesTheOperatorStationEverywhere(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	seedOldStation(t, db)

	if o, m, c, d := countOldStation(t, db); o+m+c+d != 4 {
		t.Fatalf("seed did not land: orders=%d telemetry=%d cells=%d boards=%d", o, m, c, d)
	}

	// Re-open: the versioned migrations run again, v72's post-condition sees
	// the seeded rows and fails, and the self-heal re-runs it.
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to re-run migrations: %v", err)
	}
	defer migrated.Close()

	o, m, c, d := countOldStation(t, migrated)
	if o != 0 {
		t.Errorf("%d orders row(s) still on the old station — the orphan summary groups by this column, so the station is now two rows", o)
	}
	if m != 0 {
		t.Errorf("%d mission_telemetry row(s) still on the old station", m)
	}
	if c != 0 {
		t.Errorf("%d cell_config row(s) still on the old station — its board is cross-filtered against the dashboard scope and will empty", c)
	}
	if d != 0 {
		t.Errorf("%d dashboard(s) still scoped to the old station — they match nothing and render blank", d)
	}

	// The JSON array is rewritten in place: the renamed element only, with
	// position and neighbours intact. A rewrite that dropped the others would
	// widen every board it touched.
	var stations string
	if err := migrated.QueryRow(
		`SELECT stations_json FROM dashboards WHERE name = 'v72-board'`).Scan(&stations); err != nil {
		t.Fatalf("read back board scope: %v", err)
	}
	if want := `["plant-a.line-1","core-operator","zzz"]`; stations != want {
		t.Errorf("board scope = %s, want %s", stations, want)
	}
}

// TestV72_IsSafeToRunTwice matters more here than usual. A migration whose
// post-condition fails is re-run on EVERY boot, so a data migration that is not
// safe to repeat would compound itself once per restart.
func TestV72_IsSafeToRunTwice(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	seedOldStation(t, db)

	for i := 1; i <= 3; i++ {
		again, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("migration run %d: %v", i, err)
		}
		o, m, c, d := countOldStation(t, again)
		again.Close()
		if o+m+c+d != 0 {
			t.Fatalf("run %d left old rows: orders=%d telemetry=%d cells=%d boards=%d", i, o, m, c, d)
		}
	}

	var stations string
	if err := db.QueryRow(
		`SELECT stations_json FROM dashboards WHERE name = 'v72-board'`).Scan(&stations); err != nil {
		t.Fatalf("read back board scope: %v", err)
	}
	if want := `["plant-a.line-1","core-operator","zzz"]`; stations != want {
		t.Errorf("board scope after three runs = %s, want %s", stations, want)
	}
}
