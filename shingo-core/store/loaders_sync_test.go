//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/demands"
	"shingocore/store/loaders"
)

// TestBuildDemandRegistryFromAggregate pins the Core-authored derivation of the
// threshold registry from the aggregate (threshold-to-Core): one entry per
// loader payload, carrying the station, the loader's first window node, role,
// outbound dest, and the per-payload UOP threshold the monitor compares against.
func TestBuildDemandRegistryFromAggregate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// A shared_window loader has no node of its own; it addresses pooled demand at its
	// first window node, so seed a window — without one it drives no demand.
	var ntID, winID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ('NT-BDR','t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('WIN-1',false,$1,true) RETURNING id`, ntID,
	).Scan(&winID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	id, err := db.CreateLoader(loaders.Loader{
		Name: "L", Role: loaders.RoleProduce,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentThreshold, OutboundDest: "FG-MARKET",
	})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: winID, PayloadCode: ""}); err != nil {
		t.Fatalf("seed window home: %v", err)
	}
	if err := db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-A", UOPThreshold: 100}); err != nil {
		t.Fatalf("upsert PART-A: %v", err)
	}
	if err := db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-B", UOPThreshold: 0}); err != nil {
		t.Fatalf("upsert PART-B: %v", err)
	}

	entries, err := db.BuildDemandRegistryFromAggregate("stn-1")
	if err != nil || len(entries) != 2 {
		t.Fatalf("BuildDemandRegistryFromAggregate = %d err=%v, want 2", len(entries), err)
	}
	var a *demands.RegistryEntry
	for i := range entries {
		if entries[i].PayloadCode == "PART-A" {
			a = &entries[i]
		}
	}
	if a == nil {
		t.Fatal("no PART-A entry")
	}
	if a.StationID != "stn-1" || a.CoreNodeName != "WIN-1" || a.Role != protocol.ClaimRoleProduce || a.OutboundDest != "FG-MARKET" || a.ReplenishUOPThreshold != 100 {
		t.Errorf("PART-A entry = %+v, want stn-1 / WIN-1 / produce / FG-MARKET / 100", a)
	}
}

// TestBuildLoaderInfos pins the loader → protocol projection used by the
// downward sync, in particular the identity bridge: a home's position_node_id
// is resolved to the node NAME Edge keys on.
func TestBuildLoaderInfos(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var ntID, posID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ('NT-BLI','t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('HOME-POS-1',false,$1,true) RETURNING id`, ntID,
	).Scan(&posID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	id, err := db.CreateLoader(loaders.Loader{
		Name: "L", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold,
		OutboundDest: "FG-MARKET",
	})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: posID, PayloadCode: "PART-A", UOPThreshold: 100}); err != nil {
		t.Fatalf("UpsertLoaderHome: %v", err)
	}

	infos, err := db.BuildLoaderInfos()
	if err != nil || len(infos) != 1 {
		t.Fatalf("BuildLoaderInfos = %d err=%v, want 1", len(infos), err)
	}
	li := infos[0]
	if li.LoaderKey == "" || li.Layout != "dedicated_positions" || li.Role != "produce" {
		t.Errorf("loader info = %+v", li)
	}
	if li.ConfigGen < 1 {
		t.Errorf("config_gen not carried: %d", li.ConfigGen)
	}
	if len(li.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(li.Positions))
	}
	p := li.Positions[0]
	if p.CoreNodeName != "HOME-POS-1" {
		t.Errorf("position carries %q, want the node NAME HOME-POS-1 (id→name bridge)", p.CoreNodeName)
	}
	if p.PayloadCode != "PART-A" || p.UOPThreshold != 100 {
		t.Errorf("position = %+v, want PART-A/thr100", p)
	}
	// Kind is derived from the parent loader's layout (dedicated here), stamped
	// on the wire so the Edge never sniffs an empty payload to classify.
	if p.Kind != protocol.LoaderPositionKindDedicated {
		t.Errorf("position kind = %q, want %q (derived from layout)", p.Kind, protocol.LoaderPositionKindDedicated)
	}
}
