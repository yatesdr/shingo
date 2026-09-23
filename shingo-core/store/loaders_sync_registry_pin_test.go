//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/loaders"
)

// Characterisation pins for the registry derivation (build + sync), taken
// before the derive gained its report line and its refusal. Each one is a
// derivation that yields rows, or yields none because the config genuinely has
// none; both must keep committing exactly as they do here.

func seedRegistryNode(t *testing.T, db *store.DB, typeCode, name string) int64 {
	t.Helper()
	var ntID, id int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ($1,'t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`, typeCode,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ($1,false,$2,true) RETURNING id`, name, ntID,
	).Scan(&id); err != nil {
		t.Fatalf("seed node %s: %v", name, err)
	}
	return id
}

func registryRowCount(t *testing.T, db *store.DB, stationID string) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(`SELECT count(*) FROM demand_registry WHERE station_id=$1`, stationID).Scan(&n); err != nil {
		t.Fatalf("count demand_registry: %v", err)
	}
	return n
}

func deriveAndSync(t *testing.T, db *store.DB, stationID string) ([]demands.RegistryEntry, []demands.RegistryChange) {
	t.Helper()
	entries, err := db.BuildDemandRegistryFromAggregate(stationID)
	if err != nil {
		t.Fatalf("BuildDemandRegistryFromAggregate: %v", err)
	}
	changes, err := db.SyncDemandRegistry(stationID, entries)
	if err != nil {
		t.Fatalf("SyncDemandRegistry: %v", err)
	}
	return entries, changes
}

// A dedicated loader with two payload-bearing homes and one buffer derives two
// rows. Only the home with a threshold moves (0 -> 5); the threshold-0 home is
// written but reports no change.
func TestDemandRegistryPin_DedicatedDerivesOneRowPerPayloadHome(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	a := seedRegistryNode(t, db, "NT-PIN-D", "PIN-D-A")
	b := seedRegistryNode(t, db, "NT-PIN-D", "PIN-D-B")
	buf := seedRegistryNode(t, db, "NT-PIN-D", "PIN-D-BUF")
	id, err := db.CreateLoader(loaders.Loader{Name: "PIN-D", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	for _, h := range []loaders.Home{
		{LoaderID: id, PositionNodeID: a, PayloadCode: "P-A", UOPThreshold: 5},
		{LoaderID: id, PositionNodeID: b, PayloadCode: "P-B"},
		{LoaderID: id, PositionNodeID: buf, Kind: loaders.HomeKindBuffer},
	} {
		if err := db.UpsertLoaderHome(h); err != nil {
			t.Fatalf("UpsertLoaderHome: %v", err)
		}
	}

	entries, changes := deriveAndSync(t, db, "ST-PIN-D")
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (the buffer drives no demand)", len(entries))
	}
	if n := registryRowCount(t, db, "ST-PIN-D"); n != 2 {
		t.Fatalf("registry rows = %d, want 2", n)
	}
	if len(changes) != 1 || changes[0].CoreNodeName != "PIN-D-A" || changes[0].OldThreshold != 0 || changes[0].NewThreshold != 5 {
		t.Fatalf("changes = %+v, want exactly PIN-D-A 0->5", changes)
	}
}

// Every loader archived: the derivation yields nothing because the config has
// nothing, and the sync commits that — the rows go, and the monitored one is
// reported as a change to 0.
func TestDemandRegistryPin_EveryLoaderArchivedCommitsEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	n := seedRegistryNode(t, db, "NT-PIN-AR", "PIN-AR-1")
	id, err := db.CreateLoader(loaders.Loader{Name: "PIN-AR", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n, PayloadCode: "P-AR", UOPThreshold: 8}); err != nil {
		t.Fatalf("UpsertLoaderHome: %v", err)
	}
	deriveAndSync(t, db, "ST-PIN-AR")
	if got := registryRowCount(t, db, "ST-PIN-AR"); got != 1 {
		t.Fatalf("seeded registry rows = %d, want 1", got)
	}

	if err := db.DeleteLoader(id); err != nil {
		t.Fatalf("DeleteLoader: %v", err)
	}
	entries, changes := deriveAndSync(t, db, "ST-PIN-AR")
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
	if got := registryRowCount(t, db, "ST-PIN-AR"); got != 0 {
		t.Fatalf("registry rows = %d, want 0 — an archived loader must leave the registry", got)
	}
	if len(changes) != 1 || changes[0].OldThreshold != 8 || changes[0].NewThreshold != 0 {
		t.Fatalf("changes = %+v, want P-AR 8->0", changes)
	}
}

// No payload-bearing home: the operator cleared the only home's payload. That
// is config with no demand in it, and it commits empty.
func TestDemandRegistryPin_NoPayloadBearingHomeCommitsEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	n := seedRegistryNode(t, db, "NT-PIN-NP", "PIN-NP-1")
	id, err := db.CreateLoader(loaders.Loader{Name: "PIN-NP", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n, PayloadCode: "P-NP", UOPThreshold: 3}); err != nil {
		t.Fatalf("UpsertLoaderHome: %v", err)
	}
	deriveAndSync(t, db, "ST-PIN-NP")

	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n, PayloadCode: ""}); err != nil {
		t.Fatalf("clear payload: %v", err)
	}
	entries, changes := deriveAndSync(t, db, "ST-PIN-NP")
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
	if got := registryRowCount(t, db, "ST-PIN-NP"); got != 0 {
		t.Fatalf("registry rows = %d, want 0", got)
	}
	if len(changes) != 1 || changes[0].OldThreshold != 3 || changes[0].NewThreshold != 0 {
		t.Fatalf("changes = %+v, want P-NP 3->0", changes)
	}
}
