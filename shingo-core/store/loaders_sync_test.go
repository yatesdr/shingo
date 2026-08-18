//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
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

// TestBuildDemandRegistry_ConsumeThresholdDerivesNoThreshold covers the loader
// row that the service's refusal cannot reach: one written before the refusal
// existed, or by a direct database edit.
//
// The threshold monitor's queries are role-blind — it fires on any registry row
// with replenish_uop_threshold > 0 — and the Edge drops every signal that comes
// back, because its threshold path resolves produce loaders only. Deriving a
// zero threshold stops the pointless round trip at the source.
//
// The entry itself must survive. It carries the manual_swap binding as well as
// the threshold, so skipping the loader outright would trade an inert threshold
// for a broken swap.
func TestBuildDemandRegistry_ConsumeThresholdDerivesNoThreshold(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var ntID, winID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ('NT-CT','t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('CT-WIN-1',false,$1,true) RETURNING id`, ntID,
	).Scan(&winID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	// Straight to the store, bypassing the service — that is the shape this
	// backstop is for.
	id, err := db.CreateLoader(loaders.Loader{
		Name: "CT-L", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentThreshold,
	})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: winID, PayloadCode: ""}); err != nil {
		t.Fatalf("seed window home: %v", err)
	}
	if err := db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: "PART-C", UOPThreshold: 250}); err != nil {
		t.Fatalf("upsert PART-C: %v", err)
	}

	entries, err := db.BuildDemandRegistryFromAggregate("stn-ct")
	if err != nil {
		t.Fatalf("BuildDemandRegistryFromAggregate: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.LoaderID != id {
			continue
		}
		found = true
		if e.ReplenishUOPThreshold != 0 {
			t.Errorf("consume+threshold loader derived threshold %d, want 0 — the monitor would fire signals the Edge drops",
				e.ReplenishUOPThreshold)
		}
	}
	if !found {
		t.Error("consume+threshold loader dropped from the registry entirely; it must still carry its manual_swap binding")
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

// TestBuildLoaderInfos_CarriesHomeKind pins the fact the Edge could not
// previously ask for: which of two empty-payload positions is a kept-partial
// BUFFER and which is a HOME awaiting its payload.
//
// Core has always known — bin_loader_homes.home_kind, which InSourcePool reads
// to keep an unassigned home out of the loader's source pool. It just stopped at
// this projection, so the Edge classified by empty payload and reached the
// opposite answer for an unpinned home.
func TestBuildLoaderInfos_CarriesHomeKind(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var ntID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ('NT-HK','t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	ids := map[string]int64{}
	for _, n := range []string{"HK-PINNED", "HK-UNPINNED", "HK-BUFFER"} {
		var id int64
		if err := db.DB.QueryRow(
			`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ($1,false,$2,true) RETURNING id`, n, ntID,
		).Scan(&id); err != nil {
			t.Fatalf("seed node %s: %v", n, err)
		}
		ids[n] = id
	}

	id, err := db.CreateLoader(loaders.Loader{
		Name: "L-HK", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold,
	})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	for _, h := range []loaders.Home{
		{LoaderID: id, PositionNodeID: ids["HK-PINNED"], PayloadCode: "PART-A", Kind: loaders.HomeKindHome},
		// The case the Edge got wrong: a home with no payload yet. Blank payload,
		// but NOT a buffer — InSourcePool leaves it out of the source pool.
		{LoaderID: id, PositionNodeID: ids["HK-UNPINNED"], PayloadCode: "", Kind: loaders.HomeKindHome},
		{LoaderID: id, PositionNodeID: ids["HK-BUFFER"], PayloadCode: "", Kind: loaders.HomeKindBuffer},
	} {
		if err := db.UpsertLoaderHome(h); err != nil {
			t.Fatalf("UpsertLoaderHome: %v", err)
		}
	}

	infos, err := db.BuildLoaderInfos()
	if err != nil {
		t.Fatalf("BuildLoaderInfos: %v", err)
	}
	var li *protocol.LoaderInfo
	for i := range infos {
		if infos[i].Name == "L-HK" {
			li = &infos[i]
		}
	}
	if li == nil {
		t.Fatal("loader missing from the projection")
	}
	got := map[string]string{}
	for _, p := range li.Positions {
		got[p.CoreNodeName] = p.HomeKind
	}
	for node, want := range map[string]string{
		"HK-PINNED":   protocol.LoaderHomeKindHome,
		"HK-UNPINNED": protocol.LoaderHomeKindHome,
		"HK-BUFFER":   protocol.LoaderHomeKindBuffer,
	} {
		if got[node] != want {
			t.Errorf("%s home_kind = %q, want %q", node, got[node], want)
		}
	}
	// The load-bearing pair: two positions, both blank payload, different kinds.
	// If these ever project the same value the Edge is back to guessing.
	if got["HK-UNPINNED"] == got["HK-BUFFER"] {
		t.Errorf("an unpinned home and a buffer slot both projected %q — the wire cannot tell them apart, which is the bug this field exists to fix", got["HK-BUFFER"])
	}
}

// TestBuildLoaderInfos_SkipsAHomeWhoseNodeIsGone pins the one behaviour the
// name-resolution batching could have changed silently.
//
// Member names used to be resolved a home at a time, and a home whose node had
// been deleted was skipped so the sync stayed best-effort rather than failing
// the whole node list. The batched read INNER JOINs nodes, so such a home is
// simply absent from the map — this asserts the skip survived the rewrite, and
// that the surviving positions are still projected in full.
//
// The row is created and then orphaned directly, because no service path leaves
// a home pointing at a deleted node; it is the state a manual delete or a
// half-finished rename produces, which is exactly when a sync must not break.
func TestBuildLoaderInfos_SkipsAHomeWhoseNodeIsGone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var ntID, liveID, doomedID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ('NT-GONE','t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('GONE-LIVE',false,$1,true) RETURNING id`, ntID,
	).Scan(&liveID); err != nil {
		t.Fatalf("seed live node: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('GONE-DOOMED',false,$1,true) RETURNING id`, ntID,
	).Scan(&doomedID); err != nil {
		t.Fatalf("seed doomed node: %v", err)
	}

	id, err := db.CreateLoader(loaders.Loader{
		Name: "L-GONE", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold,
	})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	for _, h := range []loaders.Home{
		{LoaderID: id, PositionNodeID: liveID, PayloadCode: "PART-LIVE", UOPThreshold: 7},
		{LoaderID: id, PositionNodeID: doomedID, PayloadCode: "PART-DOOMED"},
	} {
		if err := db.UpsertLoaderHome(h); err != nil {
			t.Fatalf("UpsertLoaderHome: %v", err)
		}
	}

	// Orphan the second home: drop the node, leave the bin_loader_homes row.
	if _, err := db.DB.Exec(`ALTER TABLE bin_loader_homes DROP CONSTRAINT IF EXISTS bin_loader_homes_position_node_id_fkey`); err != nil {
		t.Fatalf("drop fk: %v", err)
	}
	if _, err := db.DB.Exec(`DELETE FROM nodes WHERE id=$1`, doomedID); err != nil {
		t.Fatalf("delete doomed node: %v", err)
	}

	infos, err := db.BuildLoaderInfos()
	if err != nil {
		t.Fatalf("BuildLoaderInfos: %v — a vanished member node must skip that position, not fail the sync", err)
	}
	var li *protocol.LoaderInfo
	for i := range infos {
		if infos[i].Name == "L-GONE" {
			li = &infos[i]
		}
	}
	if li == nil {
		t.Fatal("loader missing from the projection entirely")
	}
	if len(li.Positions) != 1 {
		t.Fatalf("positions = %d, want 1 — the orphaned home must be skipped, the live one kept", len(li.Positions))
	}
	p := li.Positions[0]
	if p.CoreNodeName != "GONE-LIVE" || p.PayloadCode != "PART-LIVE" || p.UOPThreshold != 7 {
		t.Errorf("surviving position = %+v, want GONE-LIVE/PART-LIVE/thr7 projected in full", p)
	}
}

// TestBuildDemandRegistry_SkipsAHomeWhoseNodeIsGone is the same pin on the other
// consumer of the batched names. A registry entry addressed at a node that no
// longer exists would be a demand nothing can deliver to.
func TestBuildDemandRegistry_SkipsAHomeWhoseNodeIsGone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var ntID, liveID, doomedID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ('NT-DGONE','t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('DGONE-LIVE',false,$1,true) RETURNING id`, ntID,
	).Scan(&liveID); err != nil {
		t.Fatalf("seed live node: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('DGONE-DOOMED',false,$1,true) RETURNING id`, ntID,
	).Scan(&doomedID); err != nil {
		t.Fatalf("seed doomed node: %v", err)
	}

	id, err := db.CreateLoader(loaders.Loader{
		Name: "L-DGONE", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold,
	})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	for _, h := range []loaders.Home{
		{LoaderID: id, PositionNodeID: liveID, PayloadCode: "PART-LIVE", UOPThreshold: 5},
		{LoaderID: id, PositionNodeID: doomedID, PayloadCode: "PART-DOOMED", UOPThreshold: 9},
	} {
		if err := db.UpsertLoaderHome(h); err != nil {
			t.Fatalf("UpsertLoaderHome: %v", err)
		}
	}
	if _, err := db.DB.Exec(`ALTER TABLE bin_loader_homes DROP CONSTRAINT IF EXISTS bin_loader_homes_position_node_id_fkey`); err != nil {
		t.Fatalf("drop fk: %v", err)
	}
	if _, err := db.DB.Exec(`DELETE FROM nodes WHERE id=$1`, doomedID); err != nil {
		t.Fatalf("delete doomed node: %v", err)
	}

	entries, err := db.BuildDemandRegistryFromAggregate("edge.line1")
	if err != nil {
		t.Fatalf("BuildDemandRegistryFromAggregate: %v — a vanished node must skip its entry, not fail the derivation", err)
	}
	var mine []demands.RegistryEntry
	for _, e := range entries {
		if e.LoaderID == id {
			mine = append(mine, e)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("entries for this loader = %d, want 1 — the orphaned home drives no demand", len(mine))
	}
	if mine[0].CoreNodeName != "DGONE-LIVE" || mine[0].PayloadCode != "PART-LIVE" || mine[0].ReplenishUOPThreshold != 5 {
		t.Errorf("entry = %+v, want DGONE-LIVE/PART-LIVE/thr5", mine[0])
	}
}

// TestLoaderFunnelWindows_RoundTrip follows the window-delivery setting the whole
// way: written on the aggregate, read back off it, and projected onto the wire
// shape the Edge syncs from. A setting that survives the write but not the
// projection is invisible to the only component that acts on it.
//
// It also pins the default. A loader created without mentioning the setting must
// come back FALSE — spread — because that is what every loader did when this was
// a plant-wide config key nobody had set. A default that flipped here would
// change how live loaders are fed on the deploy that introduced the column.
func TestLoaderFunnelWindows_RoundTrip(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var ntID, winID int64
	if err := db.DB.QueryRow(
		`INSERT INTO node_types (code,name) VALUES ('NT-FW','t') ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
	).Scan(&ntID); err != nil {
		t.Fatalf("seed node_type: %v", err)
	}
	if err := db.DB.QueryRow(
		`INSERT INTO nodes (name,is_synthetic,node_type_id,enabled) VALUES ('FW-WIN-1',false,$1,true) RETURNING id`, ntID,
	).Scan(&winID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	id, err := db.CreateLoader(loaders.Loader{
		Name: "FW-L", Role: loaders.RoleProduce,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentThreshold,
	})
	if err != nil {
		t.Fatalf("CreateLoader: %v", err)
	}
	if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: winID, PayloadCode: ""}); err != nil {
		t.Fatalf("seed window home: %v", err)
	}

	got, err := db.GetLoader(id)
	if err != nil || got == nil {
		t.Fatalf("GetLoader: %v", err)
	}
	if got.FunnelWindows {
		t.Fatal("a loader created without mentioning the setting funnels; it must spread — that is what every loader did before the column existed")
	}
	if projected := findLoaderInfo(t, db, id); projected.FunnelWindows {
		t.Error("wire projection says funnel for a loader that spreads")
	}

	// Turn it on and follow it back out.
	got.FunnelWindows = true
	if err := db.UpdateLoader(*got); err != nil {
		t.Fatalf("UpdateLoader: %v", err)
	}
	back, err := db.GetLoader(id)
	if err != nil || back == nil {
		t.Fatalf("GetLoader after update: %v", err)
	}
	if !back.FunnelWindows {
		t.Error("setting did not survive the write")
	}
	if projected := findLoaderInfo(t, db, id); !projected.FunnelWindows {
		t.Error("setting survives the write but is dropped by BuildLoaderInfos — the Edge would never see it")
	}

	// ListLoaders is the enumeration the sync actually walks; a column read
	// correctly by GetLoader and dropped by the list read would sync as false.
	all, err := db.ListLoaders()
	if err != nil {
		t.Fatalf("ListLoaders: %v", err)
	}
	var seen bool
	for _, l := range all {
		if l.ID == id {
			seen = true
			if !l.FunnelWindows {
				t.Error("ListLoaders drops the setting")
			}
		}
	}
	if !seen {
		t.Fatal("loader missing from ListLoaders")
	}
}

// findLoaderInfo projects every loader and returns the one with this id's key.
func findLoaderInfo(t *testing.T, db *store.DB, id int64) protocol.LoaderInfo {
	t.Helper()
	infos, err := db.BuildLoaderInfos()
	if err != nil {
		t.Fatalf("BuildLoaderInfos: %v", err)
	}
	want := loaders.Key(id)
	for _, li := range infos {
		if li.LoaderKey == want {
			return li
		}
	}
	t.Fatalf("loader %s not in BuildLoaderInfos output", want)
	return protocol.LoaderInfo{}
}
