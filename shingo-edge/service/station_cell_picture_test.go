package service

import (
	"context"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
)

// station_cell_picture_test.go — BuildView carries the cell picture, built on
// a whole synthetic plant seeded through the store's own write paths, with the
// scene cache and the group map handed in the way the engine hands them.

func plantAStation(t *testing.T, fx scenefixtures.Plant) (*StationService, int64, testdb.SeededPlant) {
	t.Helper()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")
	stationID, ok := seeded.Stations["screen-a4"] // the press station's code in plant A
	if !ok {
		t.Fatalf("the press station is not seeded: %v", seeded.Stations)
	}
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(fx))
	svc.SetCoreNodeGroupResolver(func() map[string][]string { return testdb.GroupsOf(fx) })
	return svc, stationID, seeded
}

func cellPosition(c *domain.CellPicture, name string) *domain.CellPosition {
	for i := range c.Positions {
		if c.Positions[i].CoreNodeName == name {
			return &c.Positions[i]
		}
	}
	return nil
}

func TestBuildView_CellPictureFromAWholePlant(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	svc, stationID, seeded := plantAStation(t, fx)
	styleID := seeded.Styles["PART 40421-RVJ56.37"]
	if err := svc.db.SetActiveStyle(seeded.ProcessID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	geom := testdb.SceneGeometryOf(fx)
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return geom })

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.Cell == nil {
		t.Fatal("the view carries no cell picture")
	}
	if !view.Cell.Geometry {
		t.Error("the picture fell back to the schematic; this press has geometry for every position")
	}
	if len(view.Cell.Positions) != 6 {
		t.Fatalf("%d positions, want the six press positions: %+v", len(view.Cell.Positions), view.Cell.Positions)
	}
	front := cellPosition(view.Cell, "PLN_01")
	if front == nil || front.Role != "front" || front.Claim == nil || front.Claim.PairedCoreNode != "PLN_02" {
		t.Errorf("PLN_01 = %+v", front)
	}
	if back := cellPosition(view.Cell, "PLN_02"); back == nil || back.Role != "back" || back.PartnerOf != "PLN_01" || back.X == nil || *back.X != 135.157 {
		t.Errorf("PLN_02 = %+v, want the stationless back position on deck for PLN_01 at x=135.157", back)
	}
	// The press's shape, from every style's claims: PLN_02/05 are back slots
	// even where the running style leaves them idle; PLN_03/06 are front
	// slots even though style 7 does not run them.
	for name, kind := range map[string]string{"PLN_02": "back", "PLN_05": "back", "PLN_03": "front", "PLN_06": "front"} {
		if p := cellPosition(view.Cell, name); p == nil || p.Kind != kind {
			t.Errorf("%s kind = %+v, want %s", name, p, kind)
		}
	}
	if got := view.Cell.Groups["Supermarket Area"]; len(got) != 4 {
		t.Errorf("Supermarket Area members = %v, want four", got)
	}
}

// TestBuildView_CellPictureIsTheSchematicWithoutGeometry: no scene cached yet
// (a fresh Edge, or one that never heard from Core) still yields every
// position, in sequence order, for even spacing — never a blank panel.
func TestBuildView_CellPictureIsTheSchematicWithoutGeometry(t *testing.T) {
	t.Parallel()
	fx := scenefixtures.A()
	svc, stationID, _ := plantAStation(t, fx)
	// No geometry resolver wired at all — the lightest constructor.
	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.Cell == nil {
		t.Fatal("no cell picture without geometry — the schematic must still be drawn")
	}
	if view.Cell.Geometry {
		t.Error("geometry flagged with no scene cached")
	}
	if len(view.Cell.Positions) != 6 {
		t.Fatalf("%d positions, want the six press positions", len(view.Cell.Positions))
	}
	// Sequence order as the STORE holds it (CreateProcessNode assigns a
	// sequence to a row created with 0, so the pull's PLN_05 = 0 is not what
	// the database ends up with), name as the tiebreak; nothing placed.
	for i, p := range view.Cell.Positions {
		if p.X != nil || p.Y != nil {
			t.Errorf("%s is placed with no scene cached", p.CoreNodeName)
		}
		if i == 0 {
			continue
		}
		prev := view.Cell.Positions[i-1]
		if prev.Sequence > p.Sequence || (prev.Sequence == p.Sequence && prev.CoreNodeName > p.CoreNodeName) {
			t.Errorf("positions out of order at %d: %s(%d) before %s(%d)", i, prev.CoreNodeName, prev.Sequence, p.CoreNodeName, p.Sequence)
		}
	}
	// A resolver that answers nil (the engine before its first full sync)
	// is the same schematic.
	svc.SetSceneGeometryResolver(func() *domain.SceneGeometry { return nil })
	view, err = svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.Cell == nil || view.Cell.Geometry || len(view.Cell.Positions) != 6 {
		t.Errorf("nil geometry: %+v", view.Cell)
	}
}
