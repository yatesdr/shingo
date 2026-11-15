//go:build docker

package engine

import (
	"slices"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/scene"
)

// TestPointIndex_BuiltAtBootAndRebuiltOnSceneSync pins where the route index
// comes from: the scene already stored when the engine starts, and then the
// scene each sync stores, so a renamed point is named the new way on the next
// poll without a restart. Scene and ids invented.
func TestPointIndex_BuiltAtBootAndRebuiltOnSceneSync(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SeedScenePoint(t, db, "area-1", "LM7", "LocationMark", "")
	testdb.SeedScenePoint(t, db, "area-1", "LM31", "LocationMark", "")
	testutil.MustNoErr(t, db.UpsertSceneEdge(&scene.Edge{
		AreaName: "area-1", InstanceName: "LM7-LM31", ClassName: "DegenerateBezier",
		FromName: "LM7", ToName: "LM31", ToX: 1,
	}), "seed edge")

	eng := newTestEngine(t, db, simulator.New())
	resolve := func(ids ...string) []string {
		names, _, _ := eng.pointIndex.Load().Resolve(ids)
		return names
	}
	if got, want := resolve("7", "31"), []string{"LM7", "LM31"}; !slices.Equal(got, want) {
		t.Fatalf("at boot: route = %v, want %v from the stored scene", got, want)
	}

	// The fleet renames LM31 to AP31; the sync stores it, and the index
	// follows.
	pt := func(name string) fleet.ScenePoint {
		return fleet.ScenePoint{ClassName: "LocationMark", InstanceName: name, PropertiesJSON: "[]"}
	}
	eng.SyncScenePoints([]fleet.SceneArea{{
		Name:           "area-1",
		AdvancedPoints: []fleet.ScenePoint{pt("LM7"), pt("AP31")},
		Edges: []fleet.SceneEdge{
			{ClassName: "DegenerateBezier", InstanceName: "LM7-AP31", FromName: "LM7", ToName: "AP31", ToX: 1},
		},
	}})
	if got, want := resolve("7", "31"), []string{"LM7", "AP31"}; !slices.Equal(got, want) {
		t.Errorf("after the sync: route = %v, want %v from the scene it stored", got, want)
	}
}
