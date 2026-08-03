//go:build docker

package service_test

import (
	"fmt"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/registry"
)

// Station display names — the database half.
//
// The rendering half (a rename changes what a historical order shows, while the
// order's own field does not move) is database-free and lives in
// www/station_name_render_test.go so it runs in the default gate. What needs a
// real database is the other half of the same invariant: that the rename wrote
// ONE COLUMN OF ONE TABLE and that every station-keyed column in the schema is
// still holding the opaque key.

func enroll(t *testing.T, db *store.DB, uid, displayName string) {
	t.Helper()
	if _, err := registry.Enroll(db.DB, uid, displayName, uid); err != nil {
		t.Fatalf("Enroll %s: %v", uid, err)
	}
}

// stationColumns discovers every column whose name looks like a station.
//
// DISCOVERED, NOT LISTED. A hand-written table list cannot tell you what it
// left out, and the production census found 22 such columns across 19 tables —
// more than any document about this feature had enumerated. Querying the
// catalog means a column added next year is covered by this assertion the day
// it exists.
func stationColumns(t *testing.T, db *store.DB) [][2]string {
	t.Helper()
	rows, err := db.DB.Query(`
		SELECT table_name, column_name
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND column_name LIKE '%station%'
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("census query: %v", err)
	}
	defer rows.Close()

	var out [][2]string
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatalf("census scan: %v", err)
		}
		// edge_registry.display_name is the one place the label is allowed to
		// live; the column is not named "station" so it is not in this set
		// anyway, but the station_uid/station_id columns ON that table are, and
		// they must not hold it either.
		out = append(out, [2]string{tbl, col})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("census rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("census found no station columns at all — the assertion below would " +
			"pass vacuously, which is worse than failing")
	}
	return out
}

// TestStationRename_WritesNoStationKeyedColumn is the database half of the
// invariant.
//
// It renames a station and then asks every station-bearing column in the schema
// whether the new label leaked into it. A denormalising implementation is
// exactly what this catches, and it catches it wherever it lands rather than
// only in the tables somebody remembered to check.
func TestStationRename_WritesNoStationKeyedColumn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	ns := service.NewNodeService(db)

	const uid = "plant-a.line-1"
	const renamed = "SPRINGFIELD-WELD-CELL-RENAMED"
	enroll(t, db, uid, "SPRINGFIELD / LINE 1")

	cols := stationColumns(t, db)

	ok, err := ns.RenameEdge(uid, renamed)
	if err != nil {
		t.Fatalf("RenameEdge: %v", err)
	}
	if !ok {
		t.Fatal("RenameEdge reported no matching station")
	}

	for _, tc := range cols {
		tbl, col := tc[0], tc[1]
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE %q::text = $1`, tbl, col)
		if err := db.DB.QueryRow(q, renamed).Scan(&n); err != nil {
			t.Fatalf("%s.%s: %v", tbl, col, err)
		}
		if n != 0 {
			t.Errorf("the display name reached %s.%s in %d row(s) — renaming must write "+
				"edge_registry.display_name and nothing else; a label on a station-keyed "+
				"column is the v66 regression this split exists to prevent", tbl, col, n)
		}
	}

	// And the identity itself is untouched: the row still answers to the uid.
	e, err := ns.GetEdge(uid)
	if err != nil {
		t.Fatalf("GetEdge(%q) after rename: %v — the uid must still resolve the row", uid, err)
	}
	if e.StationUID != uid {
		t.Fatalf("StationUID = %q after a rename, want %q", e.StationUID, uid)
	}
	if e.DisplayName != renamed {
		t.Fatalf("DisplayName = %q, want %q", e.DisplayName, renamed)
	}
}

// TestStationName_RenameIsVisibleWithoutRestart pins the cache invalidation.
//
// The resolver caches a one-row map. If a rename did not drop it, every screen
// would keep showing the old name until Core restarted — which is the kind of
// defect that gets diagnosed as "the rename didn't save".
func TestStationName_RenameIsVisibleWithoutRestart(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	ns := service.NewNodeService(db)

	const uid = "stn-a1b2c3d4e5f6a7b8"
	enroll(t, db, uid, "BEFORE")

	// Resolve first, so the cache is populated and a stale read is possible.
	if got := ns.StationName(uid); got != "BEFORE" {
		t.Fatalf("StationName = %q, want %q", got, "BEFORE")
	}
	if _, err := ns.RenameEdge(uid, "AFTER"); err != nil {
		t.Fatalf("RenameEdge: %v", err)
	}
	if got := ns.StationName(uid); got != "AFTER" {
		t.Fatalf("StationName = %q after rename, want %q — the cache was not invalidated, "+
			"so the new name needs a Core restart to appear", got, "AFTER")
	}
}

// TestStationName_UnknownStationsFallBackToThemselves covers the values that
// are real in production and have no registry row: Core's own synthetic order
// sources and the broadcast address.
func TestStationName_UnknownStationsFallBackToThemselves(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	ns := service.NewNodeService(db)
	enroll(t, db, "plant-a.line-1", "SPRINGFIELD / LINE 1")

	for _, station := range []string{"core-operator", "core-direct", "core-test", "*", "stn-nope"} {
		if got := ns.StationName(station); got != station {
			t.Errorf("StationName(%q) = %q, want the identity back — an unenrolled "+
				"station must degrade to today's behaviour, not to blank", station, got)
		}
	}
	if got := ns.StationName(""); got != "" {
		t.Errorf("StationName(\"\") = %q, want \"\"", got)
	}
}

// TestStationName_IsUidFormatAgnostic pins that resolution is a whole-string
// lookup, so this code is correct whether the plants keep their backfilled
// 'plant-a.line-1' or take fresh minted uids. That decision is open elsewhere;
// this asserts it cannot reach here.
func TestStationName_IsUidFormatAgnostic(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	ns := service.NewNodeService(db)

	want := map[string]string{
		"plant-a.line-1":        "LEGACY BACKFILLED",
		"stn-a1b2c3d4e5f6a7b8":  "MINTED OPAQUE",
		"edge-a1b2c3d4e5f6a7b8": "MINTED ALTERNATE",
	}
	for uid, name := range want {
		enroll(t, db, uid, name)
	}
	for uid, name := range want {
		if got := ns.StationName(uid); got != name {
			t.Errorf("StationName(%q) = %q, want %q", uid, got, name)
		}
	}

	all := ns.StationNames()
	for uid, name := range want {
		if all[uid] != name {
			t.Errorf("StationNames()[%q] = %q, want %q", uid, all[uid], name)
		}
	}

	// The returned map is a copy: mutating it must not corrupt the cache every
	// other in-flight request is reading.
	all["plant-a.line-1"] = "MUTATED"
	if got := ns.StationName("plant-a.line-1"); got != "LEGACY BACKFILLED" {
		t.Fatalf("mutating the StationNames() result changed the resolver: got %q", got)
	}
}
