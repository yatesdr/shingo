//go:build docker

package loaders_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
)

// seedNode inserts a node_type + node so bin_loader_homes.position_node_id has a
// valid FK target. Mirrors the minimal insert in store/nodes/group.go.
func seedNode(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	var ntID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code, name) VALUES ($1,$1) ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
		"NT-"+name).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	var nodeID int64
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name, is_synthetic, node_type_id, enabled) VALUES ($1, false, $2, true) RETURNING id`,
		name, ntID).Scan(&nodeID); err != nil {
		t.Fatalf("seed node %s: %v", name, err)
	}
	return nodeID
}

func produceLoader(name string) loaders.Loader {
	return loaders.Loader{
		Name: name, Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold,
		OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
	}
}

func TestLoaderCRUD(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	id, err := db.CreateLoader(produceLoader("LDR-1"))
	if err != nil || id == 0 {
		t.Fatalf("CreateLoader: id=%d err=%v", id, err)
	}

	got, err := db.GetLoader(id)
	if err != nil || got == nil {
		t.Fatalf("GetLoader: %v", err)
	}
	if got.Name != "LDR-1" || got.Layout != loaders.LayoutDedicatedPositions {
		t.Errorf("GetLoader = %+v, want LDR-1 / dedicated_positions", got)
	}
	if got.ConfigGen != 1 {
		t.Errorf("fresh loader config_gen = %d, want 1", got.ConfigGen)
	}

	byName, err := db.GetLoaderByName("LDR-1", loaders.RoleProduce)
	if err != nil || byName == nil || byName.ID != id {
		t.Errorf("GetLoaderByName = %+v err=%v, want id=%d", byName, err, id)
	}

	upd := *got
	upd.Layout = loaders.LayoutSharedWindow
	upd.OutboundDest = "OVERFLOW-MARKET"
	if err := db.UpdateLoader(upd); err != nil {
		t.Fatalf("UpdateLoader: %v", err)
	}
	got2, _ := db.GetLoader(id)
	if got2.Layout != loaders.LayoutSharedWindow || got2.OutboundDest != "OVERFLOW-MARKET" {
		t.Errorf("after update = %+v, want shared_window / OVERFLOW-MARKET", got2)
	}
	if got2.ConfigGen <= got.ConfigGen {
		t.Errorf("config_gen did not advance on update: %d -> %d", got.ConfigGen, got2.ConfigGen)
	}

	all, err := db.ListLoaders()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListLoaders = %d err=%v, want 1", len(all), err)
	}

	if err := db.DeleteLoader(id); err != nil {
		t.Fatalf("DeleteLoader: %v", err)
	}
	// Soft-delete (step 7): the row survives (its non-cascading bin_uop_audit loader_id
	// history must outlive the loader), but it is archived and excluded from the active
	// enumeration. Get* are raw lookups and still return it (with archived_at set).
	archived, err := db.GetLoader(id)
	if err != nil || archived == nil {
		t.Fatalf("GetLoader after soft-delete = %+v err=%v, want the archived row to survive", archived, err)
	}
	if archived.ArchivedAt == nil {
		t.Error("DeleteLoader should set archived_at (soft-delete), got nil")
	}
	if all, err := db.ListLoaders(); err != nil || len(all) != 0 {
		t.Errorf("ListLoaders after soft-delete = %d err=%v, want 0 (archived excluded from active config)", len(all), err)
	}
}

// TestHomeOnePayloadPerPosition pins the load-bearing invariant: a position node
// belongs to exactly one loader and carries exactly one payload
// (UNIQUE(position_node_id)). Re-assigning a position replaces it; moving it to
// another loader relocates the single row. Same payload on two DIFFERENT
// positions is allowed (D1).
func TestHomeOnePayloadPerPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	loaderA, _ := db.CreateLoader(produceLoader("A"))
	loaderB, _ := db.CreateLoader(produceLoader("B"))
	pos1 := seedNode(t, db, "POS-1")
	pos2 := seedNode(t, db, "POS-2")

	// Assign pos1=PART-A to loaderA.
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: loaderA, PositionNodeID: pos1, PayloadCode: "PART-A"}); err != nil {
		t.Fatalf("upsert pos1=PART-A: %v", err)
	}
	// Re-assign pos1=PART-B (same loader) → replaces, still one row.
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: loaderA, PositionNodeID: pos1, PayloadCode: "PART-B"}); err != nil {
		t.Fatalf("upsert pos1=PART-B: %v", err)
	}
	homesA, _ := db.ListLoaderHomes(loaderA)
	if len(homesA) != 1 || homesA[0].PayloadCode != "PART-B" {
		t.Fatalf("loaderA homes = %+v, want one row PART-B", homesA)
	}

	// Same payload on a SECOND position of loaderA is allowed (D1).
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: loaderA, PositionNodeID: pos2, PayloadCode: "PART-B"}); err != nil {
		t.Fatalf("upsert pos2=PART-B (D1): %v", err)
	}
	if homesA, _ = db.ListLoaderHomes(loaderA); len(homesA) != 2 {
		t.Errorf("loaderA homes = %d, want 2 (same payload, two positions allowed)", len(homesA))
	}

	// Moving pos1 to loaderB relocates the single position row globally.
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: loaderB, PositionNodeID: pos1, PayloadCode: "PART-C"}); err != nil {
		t.Fatalf("move pos1 to loaderB: %v", err)
	}
	homesA, _ = db.ListLoaderHomes(loaderA)
	homesB, _ := db.ListLoaderHomes(loaderB)
	if len(homesA) != 1 || homesA[0].PositionNodeID != pos2 {
		t.Errorf("loaderA homes after move = %+v, want only pos2", homesA)
	}
	if len(homesB) != 1 || homesB[0].PositionNodeID != pos1 || homesB[0].PayloadCode != "PART-C" {
		t.Errorf("loaderB homes after move = %+v, want pos1=PART-C", homesB)
	}
}

func TestPayloadSetAndConfigGen(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	id, _ := db.CreateLoader(produceLoader("SW"))

	gen := func() int64 { l, _ := db.GetLoader(id); return l.ConfigGen }
	g0 := gen()

	if err := db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-X", UOPThreshold: 100}); err != nil {
		t.Fatalf("upsert PART-X: %v", err)
	}
	if gen() <= g0 {
		t.Errorf("config_gen did not advance on payload add")
	}
	// Update the same payload (threshold change).
	if err := db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-X", UOPThreshold: 250}); err != nil {
		t.Fatalf("update PART-X: %v", err)
	}
	if err := db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-Y"}); err != nil {
		t.Fatalf("upsert PART-Y: %v", err)
	}
	ps, _ := db.ListLoaderPayloads(id)
	if len(ps) != 2 {
		t.Fatalf("payloads = %d, want 2", len(ps))
	}
	if ps[0].PayloadCode != "PART-X" || ps[0].UOPThreshold != 250 {
		t.Errorf("PART-X = %+v, want threshold 250 (upsert updated)", ps[0])
	}
	if err := db.RemoveLoaderPayload(id, "PART-Y"); err != nil {
		t.Fatalf("remove PART-Y: %v", err)
	}
	if ps, _ = db.ListLoaderPayloads(id); len(ps) != 1 {
		t.Errorf("payloads after remove = %d, want 1", len(ps))
	}
}

// TestCascadeDelete verifies ON DELETE CASCADE drops homes + payloads with the
// loader (and frees the position for reassignment).
func TestCascadeDelete(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	id, _ := db.CreateLoader(produceLoader("C"))
	pos := seedNode(t, db, "POS-C")
	_ = db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: pos, PayloadCode: "PART-A"})
	_ = db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-A"})

	cfg, err := db.GetLoaderConfig(id)
	if err != nil || cfg == nil || len(cfg.Homes) != 1 || len(cfg.Payloads) != 1 {
		t.Fatalf("GetLoaderConfig = %+v err=%v, want 1 home + 1 payload", cfg, err)
	}

	if err := db.DeleteLoader(id); err != nil {
		t.Fatalf("DeleteLoader: %v", err)
	}
	// Position is free again — a new loader can claim it.
	id2, _ := db.CreateLoader(produceLoader("C2"))
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id2, PositionNodeID: pos, PayloadCode: "PART-Z"}); err != nil {
		t.Errorf("position not freed by cascade delete: %v", err)
	}
}
