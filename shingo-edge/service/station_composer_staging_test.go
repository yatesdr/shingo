package service

import (
	"encoding/json"
	"slices"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
)

// station_composer_staging_test.go — the composer's own picture, and what it
// costs.
//
// THE COMPOSER DREW THE BOARD'S PICTURE. `buildModel` read the POLLED view's
// cell, which is the board's: the staging the RUNNING flow parks at, and
// nothing the cell merely could park at. So the one screen whose job is
// choosing where a bin goes was the surface with no options drawn on it —
// the opposite of the complaint this work started from.

func stagingComposerFixture(t *testing.T) (*StationService, int64, testdb.SeededPlant) {
	t.Helper()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	stationID, ok := seeded.Stations["screen-a4"]
	if !ok {
		t.Fatalf("the press station is not seeded: %v", seeded.Stations)
	}
	svc := NewStationService(db)
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	return svc, stationID, seeded
}

func TestComposerForStation_CarriesItsOwnPictureWithTheStagingItMayUse(t *testing.T) {
	t.Parallel()
	svc, stationID, seeded := stagingComposerFixture(t)

	// A staging lane in the routing set that NO flow uses: the composer must
	// offer it, and the board must not draw it.
	if _, err := svc.db.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: seeded.ProcessID, CoreNodeName: "SLN_OFFERED",
		Role: domain.RoutingRoleStaging, Enabled: true, Origin: domain.RoutingOriginEngineer,
	}); err != nil {
		t.Fatalf("seed the offered lane: %v", err)
	}

	data, err := svc.ComposerForStation(stationID)
	if err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	if data.Cell == nil {
		t.Fatal("the station's composer read carries no picture; buildModel would draw the board's")
	}
	names := make([]string, 0, len(data.Cell.Staging))
	for _, st := range data.Cell.Staging {
		names = append(names, st.CoreNodeName)
	}
	if !slices.Contains(names, "SLN_OFFERED") {
		t.Errorf("the composer's picture carries staging %v, without the lane the routing set "+
			"offers — the screen where a lane is chosen draws no lane to choose", names)
	}
	// NOT A POSITION, which is the rule the separate list exists to make true.
	for _, p := range data.Cell.Positions {
		if p.CoreNodeName == "SLN_OFFERED" {
			t.Error("the offered lane is drawn as a position an operator may claim")
		}
	}

	// AND THE BOARD'S PICTURE DOES NOT CARRY IT. The board draws what the flow
	// IS; the composer draws what it could be.
	board, err := svc.CellPictureForStation(stationID)
	if err != nil {
		t.Fatalf("CellPictureForStation: %v", err)
	}
	for _, st := range board.Staging {
		if st.CoreNodeName == "SLN_OFFERED" {
			t.Error("the board's picture draws a staging lane the running flow does not use")
		}
	}
}

// THE PICTURE COSTS THE COMPOSER READ NOTHING IT WAS NOT ALREADY PAYING.
//
// Its node list is the one the preset order already takes, and its back
// positions are a fold over the claims in hand — so the whole picture, staging
// band and LM points included, is zero extra queries on a read that happens
// once when an operator taps CHANGEOVER.
func TestComposerForStation_ThePictureAddsNoQueries(t *testing.T) {
	fx := seedBudgetFixture(t)

	fx.counter.Reset()
	withPicture, err := fx.svc.ComposerForStation(fx.stationID)
	if err != nil {
		t.Fatalf("ComposerForStation: %v", err)
	}
	queries := fx.counter.Count()
	if withPicture.Cell == nil {
		t.Fatal("no picture on the station's composer read")
	}
	// The pin the brief asks for: the budget this read has always had.
	const budget = 12
	if queries > budget {
		t.Errorf("the station's composer read issues %d queries with its picture, budget %d", queries, budget)
	}

	// AND IT IS SMALL. The picture is one cell — six positions, its staging
	// band and the waypoints inside its own region — not the plant.
	raw, err := json.Marshal(withPicture.Cell)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const pictureBudget = 8 * 1024
	if len(raw) > pictureBudget {
		t.Errorf("the composer's picture serialises to %d bytes, budget %d — it is one cell, and if "+
			"it has grown into a map it is on a screen the station fetches over plant WiFi",
			len(raw), pictureBudget)
	}
	t.Logf("station composer read: %d queries, picture %d bytes of %d", queries, len(raw), pictureBudget)
}
