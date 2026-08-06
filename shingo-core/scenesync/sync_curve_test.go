package scenesync

import (
	"testing"

	"shingocore/fleet"
)

// TestSyncScenePoints_CarriesControlHandles pins the middle link of the chain
// that ends at the map: a curved lane's handles must reach the scene_edges row,
// and a straight lane must reach it with all four NULL.
//
// The straight case is the one worth stating. Every write path in this file
// names the columns it sets, so "the handles are absent" is only ever true if
// something actively refrains from writing them — and the failure mode of
// getting that wrong is not an empty map, it is a confident wrong one.
func TestSyncScenePoints_CarriesControlHandles(t *testing.T) {
	t.Parallel()
	db := newFakeStore()

	areas := []fleet.SceneArea{{
		Name: "SPR",
		Edges: []fleet.SceneEdge{
			{
				InstanceName: "LM10-LM113", ClassName: "DegenerateBezier",
				FromName: "LM10", ToName: "LM113",
				FromX: -0.254, FromY: 33.554, ToX: -6.572, ToY: 36.951,
				Ctrl1: &fleet.ScenePos{X: -0.198, Y: 36.401},
				Ctrl2: &fleet.ScenePos{X: -5.065, Y: 36.951},
			},
			{
				InstanceName: "LM100-AP102", ClassName: "StraightPath",
				FromName: "LM100", ToName: "AP102",
				FromX: -0.544, FromY: 11.787, ToX: 0.886, ToY: 11.807,
			},
			// Half a pair is not a curve. Nothing upstream produces this
			// today; the guard is here because three of four numbers would
			// otherwise reach the renderer, which has to invent the fourth.
			{
				InstanceName: "LM1-LM2", ClassName: "DegenerateBezier",
				FromName: "LM1", ToName: "LM2",
				FromX: 1, FromY: 1, ToX: 2, ToY: 2,
				Ctrl1: &fleet.ScenePos{X: 1.5, Y: 9},
			},
		},
	}}

	SyncScenePoints(db, noopLog, areas, "", nil)

	curved := db.edges["SPR/LM10-LM113"]
	if curved == nil {
		t.Fatal("curved edge was not stored")
	}
	if !curved.Curved() {
		t.Fatalf("curved edge stored without handles: %+v", curved)
	}
	if *curved.Ctrl1X != -0.198 || *curved.Ctrl1Y != 36.401 ||
		*curved.Ctrl2X != -5.065 || *curved.Ctrl2Y != 36.951 {
		t.Errorf("handles = (%v,%v)/(%v,%v), want (-0.198,36.401)/(-5.065,36.951)",
			*curved.Ctrl1X, *curved.Ctrl1Y, *curved.Ctrl2X, *curved.Ctrl2Y)
	}

	straight := db.edges["SPR/LM100-AP102"]
	if straight == nil {
		t.Fatal("straight edge was not stored")
	}
	if straight.Curved() {
		t.Errorf("straight aisle stored with handles (%v,%v)/(%v,%v)",
			straight.Ctrl1X, straight.Ctrl1Y, straight.Ctrl2X, straight.Ctrl2Y)
	}

	partial := db.edges["SPR/LM1-LM2"]
	if partial == nil {
		t.Fatal("partial-handle edge was not stored")
	}
	if partial.Curved() || partial.Ctrl1X != nil {
		t.Errorf("half a handle pair became a curve: %+v", partial)
	}
}
