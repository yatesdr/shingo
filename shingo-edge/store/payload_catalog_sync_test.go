package store

import (
	"testing"

	"shingoedge/store/catalog"
)

// The catalog sync path: one transaction for the whole loop, a conditional
// upsert that leaves unchanged rows (and their updated_at) alone, and a prune
// that is atomic with the upserts.

func TestSyncPayloadCatalog_OneTxConditionalUpsertAndPrune(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Seed: two existing rows, one about to change, one not; plus one stale
	// row the sync should prune.
	seed := func() {
		for _, e := range []*catalog.CatalogEntry{
			{ID: 1, Name: "Bracket", Code: "BRK", Description: "v1", UOPCapacity: 100, CATID: "C1"},
			{ID: 2, Name: "Tote", Code: "TOT", Description: "fixed", UOPCapacity: 50, CATID: "C2"},
			{ID: 99, Name: "Stale", Code: "STALE", UOPCapacity: 10},
		} {
			if err := db.UpsertPayloadCatalog(e); err != nil {
				t.Fatalf("seed %s: %v", e.Code, err)
			}
		}
	}
	seed()

	// Record the unchanged row's updated_at — the conditional upsert must not
	// move it.
	var before string
	if err := db.QueryRow(`SELECT updated_at FROM payload_catalog WHERE id=2`).Scan(&before); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	// Sync: id 1 CHANGED (description v1→v2), id 2 identical, id 3 NEW, id 99
	// absent → pruned.
	err := db.SyncPayloadCatalog([]*catalog.CatalogEntry{
		{ID: 1, Name: "Bracket", Code: "BRK", Description: "v2", UOPCapacity: 100, CATID: "C1"},
		{ID: 2, Name: "Tote", Code: "TOT", Description: "fixed", UOPCapacity: 50, CATID: "C2"},
		{ID: 3, Name: "Panel", Code: "PNL", Description: "", UOPCapacity: 75, CATID: ""},
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payload_catalog`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows after sync = %d, want 3 (stale pruned, new inserted)", n)
	}

	var desc string
	if err := db.QueryRow(`SELECT description FROM payload_catalog WHERE id=1`).Scan(&desc); err != nil {
		t.Fatalf("read changed row: %v", err)
	}
	if desc != "v2" {
		t.Errorf("changed row description = %q, want v2", desc)
	}

	var after string
	if err := db.QueryRow(`SELECT updated_at FROM payload_catalog WHERE id=2`).Scan(&after); err != nil {
		t.Fatalf("re-read updated_at: %v", err)
	}
	if after != before {
		t.Errorf("unchanged row updated_at moved: %q → %q (conditional upsert must not stamp it)", before, after)
	}

	var stale int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payload_catalog WHERE id=99`).Scan(&stale); err != nil {
		t.Fatalf("count stale: %v", err)
	}
	if stale != 0 {
		t.Errorf("stale row survived the sync")
	}
}

// The re-sync of an identical catalog must be a pure no-op — updated_at
// untouched on every row.
func TestSyncPayloadCatalog_IdenticalResyncIsANoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	entries := []*catalog.CatalogEntry{
		{ID: 1, Name: "Bracket", Code: "BRK", Description: "d", UOPCapacity: 100, CATID: "C1"},
		{ID: 2, Name: "Tote", Code: "TOT", Description: "d2", UOPCapacity: 50, CATID: "C2"},
	}
	if err := db.SyncPayloadCatalog(entries); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	var stamps []string
	rows, err := db.Query(`SELECT updated_at FROM payload_catalog ORDER BY id`)
	if err != nil {
		t.Fatalf("read stamps: %v", err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stamps = append(stamps, s)
	}
	rows.Close()

	if err := db.SyncPayloadCatalog(entries); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	var stamps2 []string
	rows2, err := db.Query(`SELECT updated_at FROM payload_catalog ORDER BY id`)
	if err != nil {
		t.Fatalf("re-read stamps: %v", err)
	}
	for rows2.Next() {
		var s string
		if err := rows2.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stamps2 = append(stamps2, s)
	}
	rows2.Close()

	for i := range stamps {
		if stamps[i] != stamps2[i] {
			t.Errorf("row %d updated_at changed on identical re-sync: %q → %q", i, stamps[i], stamps2[i])
		}
	}
}
