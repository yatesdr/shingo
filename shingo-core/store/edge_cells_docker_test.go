//go:build docker

package store_test

import (
	"encoding/json"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestEdgeCellsDigest_KeptWithTheLiveCells: a catalog applied with its digest
// leaves that digest on every cell it made live, and that is the digest Core
// holds for the station; a later catalog replaces it and makes the cells it
// lacks stale; an empty catalog leaves the station no live cell, and Core
// holds no catalog; live cells that carry different digests, or the default
// empty digest of a cell made live before v187, read as held with no digest, which
// matches no Edge's. Another station's cells are never read.
func TestEdgeCellsDigest_KeptWithTheLiveCells(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	cell := func(label string) store.EdgeCell {
		return store.EdgeCell{CellLabel: label, Bindings: json.RawMessage(`[{"process_id":"P1"}]`)}
	}
	held := func(when, station, wantDigest string, wantHeld bool) {
		t.Helper()
		d, h, err := db.EdgeCellsDigest(station)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if d != wantDigest || h != wantHeld {
			t.Errorf("%s: EdgeCellsDigest(%s) = (%q, %v), want (%q, %v)", when, station, d, h, wantDigest, wantHeld)
		}
	}
	live := func(when, station string, want int) {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM edge_cells WHERE station_uid = $1 AND NOT stale`, station).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s: %s has %d live cells, want %d", when, station, n, want)
		}
	}

	held("nothing applied", "stn-a", "", false)
	if err := db.UpsertEdgeCells("stn-a", []store.EdgeCell{cell("PLC-1"), cell("PLC-2")}, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertEdgeCells("stn-b", []store.EdgeCell{cell("PLC-1")}, "e1"); err != nil {
		t.Fatal(err)
	}
	held("a two-cell catalog", "stn-a", "d1", true)
	held("another station's", "stn-b", "e1", true)

	if err := db.UpsertEdgeCells("stn-a", []store.EdgeCell{cell("PLC-2")}, "d2"); err != nil {
		t.Fatal(err)
	}
	held("a smaller catalog", "stn-a", "d2", true)
	live("a smaller catalog", "stn-a", 1)

	if _, err := db.Exec(`UPDATE edge_cells SET stale = FALSE, catalog_digest = '' WHERE station_uid = 'stn-a' AND cell_label = 'PLC-1'`); err != nil {
		t.Fatal(err)
	}
	held("live cells of two digests", "stn-a", "", true)

	if err := db.UpsertEdgeCells("stn-a", nil, "d-empty"); err != nil {
		t.Fatal(err)
	}
	held("an empty catalog", "stn-a", "", false)
	live("an empty catalog", "stn-a", 0)
	held("the other station, untouched", "stn-b", "e1", true)
}
