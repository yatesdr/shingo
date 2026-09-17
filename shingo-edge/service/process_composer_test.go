package service

import (
	"context"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
)

// process_composer_test.go — the desktop's read.
//
// U8's composer block is built per station because the HMI is a station's
// screen. The desktop is the same process seen whole: an engineer editing
// Press 400's flow is not standing at one of its screens, and a position bound
// to a station they are not looking at is still part of the press.
//
// So this pins the ONE thing that differs — the picture's scope — and the one
// thing that must not differ: everything else is the same builder and the same
// bytes the station gets, because "the desktop and the HMI are one thing
// managed in one place" is only true if there is one builder behind both.
func TestComposerForProcess_IsTheWholePress(t *testing.T) {
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })
	eleven := seeded.Styles["PART SYN-A-S007"]
	if err := db.SetActiveStyle(seeded.ProcessID, &eleven); err != nil {
		t.Fatalf("set active style: %v", err)
	}

	got, err := svc.ComposerForProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	if got == nil {
		t.Fatal("no composer data for the process")
	}

	// THE PICTURE IS THE WHOLE PRESS. The station view's picture carries the
	// positions bound to that station plus whatever the running style pairs
	// with; the desktop's carries every live position of the process, because
	// the engineer's table has a row for each of them and "+ Add a position"
	// offers the free ones.
	if got.Cell == nil {
		t.Fatal("composer.cell is nil — the desktop has no picture to draw")
	}
	names := map[string]bool{}
	for _, p := range got.Cell.Positions {
		names[p.CoreNodeName] = true
	}
	for _, want := range []string{"PLN_01", "PLN_02", "PLN_03", "PLN_04", "PLN_05", "PLN_06"} {
		if !names[want] {
			t.Errorf("position %s is missing from the desktop's picture (got %v)", want, keysOf(names))
		}
	}

	// AND IT IS THE SAME BUILDER. Everything the station gets, the desktop
	// gets: same styles, same claim counts, same routing set, same scene.
	station, err := svc.BuildView(context.Background(), seeded.Stations["screen-a4"])
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if station.Composer == nil {
		t.Fatal("the station view lost its composer block")
	}
	if len(got.Styles) != len(station.Composer.Styles) {
		t.Errorf("desktop sees %d styles, the station sees %d — one builder, one answer",
			len(got.Styles), len(station.Composer.Styles))
	}
	if len(got.Routing) != len(station.Composer.Routing) {
		t.Errorf("desktop sees %d routing rows, the station sees %d",
			len(got.Routing), len(station.Composer.Routing))
	}
	for i := range got.Styles {
		a, b := got.Styles[i], station.Composer.Styles[i]
		if a.ID != b.ID || a.ClaimCount != b.ClaimCount {
			t.Errorf("style %d differs between the two reads: desktop %+v, station %+v", i, a, b)
		}
	}
}

// TestComposerForProcess_CarriesAdvancedAndTheStationDoesNot — the Advanced
// modal's read (U9b).
//
// The desktop opens the modal on what the row holds, so the block has to be on
// the view; the station has no such modal, and a screen on a Pi over plant
// WiFi should not carry a policy block per position per style it will never
// draw. That asymmetry is deliberate and this is where it is written down.
func TestComposerForProcess_CarriesAdvancedAndTheStationDoesNot(t *testing.T) {
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })

	got, err := svc.ComposerForProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	withClaims, withAdvanced := 0, 0
	for _, st := range got.Styles {
		if st.ClaimCount == 0 {
			if len(st.Advanced) != 0 {
				t.Errorf("style %q has no claims but carries %d advanced rows", st.Name, len(st.Advanced))
			}
			continue
		}
		withClaims++
		if len(st.Advanced) != st.ClaimCount {
			t.Errorf("style %q: %d advanced rows for %d claims — the modal has a position it cannot open",
				st.Name, len(st.Advanced), st.ClaimCount)
			continue
		}
		withAdvanced++
		// Keyed by position, and every cell has one.
		for _, cell := range st.Claims {
			if _, ok := st.Advanced[cell.CoreNodeName]; !ok {
				t.Errorf("style %q: no advanced row for position %s", st.Name, cell.CoreNodeName)
			}
			// The cell itself stays silent about them — a cell is what a save
			// sends back, and a save speaks Advanced only when the engineer
			// opened the modal.
			if cell.Advanced != nil {
				t.Errorf("style %q position %s: Collapse filled the cell's Advanced", st.Name, cell.CoreNodeName)
			}
		}
	}
	if withClaims == 0 || withAdvanced != withClaims {
		t.Fatalf("%d styles with claims, %d with advanced — the fixture proves nothing", withClaims, withAdvanced)
	}

	station, err := svc.BuildView(context.Background(), seeded.Stations["screen-a4"])
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	for _, st := range station.Composer.Styles {
		if len(st.Advanced) != 0 {
			t.Errorf("the station view carries %d advanced rows for style %q; it has no modal to draw them in",
				len(st.Advanced), st.Name)
		}
	}
}

// TestComposerForProcess_CarriesTheMapAndTheStationDoesNot — D3's read (U9c).
//
// The routing-set screen draws the plant, so the desktop's read carries the
// geometry cache whole. The station's does not: the same asymmetry as the
// Advanced blocks, and for a bigger reason — this is every point and every
// segment of the map, and no station screen draws one.
func TestComposerForProcess_CarriesTheMapAndTheStationDoesNot(t *testing.T) {
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })

	got, err := svc.ComposerForProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForProcess: %v", err)
	}
	if got.Map == nil {
		t.Fatal("the desktop read carries no map — D3 has nothing to draw")
	}
	if len(got.Map.Points) != len(geom.Points) || len(got.Map.Edges) != len(geom.Edges) {
		t.Errorf("map is %d points / %d edges, cache is %d / %d — D3 draws the plant, not a subgraph",
			len(got.Map.Points), len(got.Map.Edges), len(geom.Points), len(geom.Edges))
	}
	if got.Map.Revision != geom.Revision {
		t.Errorf("map revision %q, cache %q", got.Map.Revision, geom.Revision)
	}

	// THE JOIN RULE TRAVELS. A press position is the GeneralLocation of that
	// name; if the class did not come with the point, the page would have to
	// guess which of two points half a metre apart is the bin location.
	for _, want := range []string{"PLN_01", "PLN_04"} {
		p, ok := got.Map.Points[want]
		if !ok {
			t.Errorf("%s is not on the map the desktop draws", want)
			continue
		}
		if p.Class != "GeneralLocation" {
			t.Errorf("%s came through as class %q", want, p.Class)
		}
		x, y, placed := domain.LocateCellPosition(geom, want)
		if !placed || p.X != x || p.Y != y {
			t.Errorf("%s is at (%v,%v) on the map and (%v,%v) in the cache", want, p.X, p.Y, x, y)
		}
	}

	// A curved segment keeps all four handles or none; three describe no cubic.
	for _, e := range got.Map.Edges {
		if e.Handles != nil && len(*e.Handles) != 4 {
			t.Fatalf("edge %s→%s carries %d handles", e.From, e.To, len(*e.Handles))
		}
	}

	// SCENE OR MAP, NEVER BOTH. They are the same network — Scene is Map with
	// the coordinates collapsed into one length per edge — so the desktop
	// read, which has Map, must not also carry Scene, and every Map edge has
	// to carry the length a waypoint walk would otherwise have needed Scene
	// for.
	if got.Scene != nil {
		t.Error("the desktop read carries Scene beside Map — the same network twice")
	}
	for _, e := range got.Map.Edges {
		// A degenerate segment has length 0 honestly. The endpoints are read
		// from Points now — an edge carries names, not places
		// (composer_map_endpoints_test.go).
		if got.Map.Points[e.From] == got.Map.Points[e.To] {
			continue
		}
		if e.Len == 0 {
			t.Fatalf("map edge %s→%s carries no length; the via walk would have to recompute every one in the browser", e.From, e.To)
		}
	}

	station, err := svc.BuildView(context.Background(), seeded.Stations["screen-a4"])
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if station.Composer.Map != nil {
		t.Errorf("the station view carries the whole plant map; no screen on it draws one")
	}
	// THE POLL CARRIES NEITHER (S8). The board reads the composer's half once,
	// on a tap; the scene rode every 500 ms poll to be discarded.
	if station.Composer.Scene != nil {
		t.Error("the station VIEW carries the travel graph — it belongs on the composer's own read")
	}

	// And the station's own composer read has it, which is where the waypoint
	// walk gets it from.
	stn, err := svc.ComposerForStation(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	if stn.Scene == nil || len(stn.Scene.Edges) == 0 {
		t.Error("the station lost the scene adjacency it walks for waypoints")
	}
	if stn.Map != nil {
		t.Error("the station read carries the whole plant map; no screen on it draws one")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestComposerReads_FirstRunHasNoRoutingSetAndNoPresets: the state every new
// line boots in, which nothing else here exercises.
//
// Every other test in this package derives a routing set and seeds presets
// first, because that is what a test wants in order to assert something about
// them. A press on the day it is commissioned has neither: no routing rows
// (nobody has adopted a backfill), no presets (nobody has named a shape), and
// no scene (Core has not synced one). Those are three empty collections the
// desktop and the station both read and the JS both indexes, and an empty
// collection is where a nil-vs-[] mistake lives — it renders as a blank tab or
// throws, on the first screen an engineer ever opens.
//
// It also pins the short-circuit's other half: composerPresets must not go
// looking for member counts when there are no presets to count.
func TestComposerReads_FirstRunHasNoRoutingSetAndNoPresets(t *testing.T) {
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	// NO SetSceneGeometryResolver, NO DeriveRoutingNodes, NO presets — the
	// three things a commissioning engineer has not done yet.

	routing, err := db.ListRoutingNodes(seeded.ProcessID)
	if err != nil {
		t.Fatalf("list routing set: %v", err)
	}
	if len(routing) != 0 {
		t.Fatalf("the fixture already has %d routing rows; this test is not about a first run", len(routing))
	}

	desktop, err := svc.ComposerForProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForProcess on a fresh press: %v", err)
	}
	if len(desktop.Styles) == 0 {
		t.Error("a fresh press offers no styles to edit — the page has nothing to draw")
	}
	if len(desktop.Routing) != 0 {
		t.Errorf("a fresh press offers %d routing options; nobody has adopted one", len(desktop.Routing))
	}
	if len(desktop.Presets) != 0 {
		t.Errorf("a fresh press offers %d preset cards", len(desktop.Presets))
	}
	// THE MAP IS ABSENT, NOT EMPTY. With no geometry cached there is no plant
	// map to draw, and the desktop's picture falls back to the schematic. An
	// empty-but-present Map is a frame with no points in it.
	if desktop.Map != nil && len(desktop.Map.Points) == 0 {
		t.Error("a fresh press ships an empty plant map; with no geometry it should ship none")
	}

	station, err := svc.ComposerForStation(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ComposerForStation on a fresh press: %v", err)
	}
	if station.Map != nil {
		t.Error("the station's read carries a plant map")
	}
	if len(station.Presets) != 0 {
		t.Errorf("a fresh press offers the station %d preset cards", len(station.Presets))
	}

	// And the view the boards poll builds, which is the screen that is actually
	// up on the floor while the engineer is still setting the press up.
	stationID, ok := seeded.Stations["screen-a4"]
	if !ok {
		t.Fatal("the fixture seeds no station")
	}
	if _, err := svc.BuildView(context.Background(), stationID); err != nil {
		t.Fatalf("BuildView on a fresh press: %v", err)
	}
}
