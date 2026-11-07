package testdb

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/scene"
)

// SeedScenePoint writes one scene row. A GeneralLocation carries the station in
// instance_name and the point the robot reports in point_name; every other
// class carries only its instance name, exactly as the sync writes them.
func SeedScenePoint(t *testing.T, db *store.DB, area, instance, class, point string) {
	t.Helper()
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
		AreaName: area, InstanceName: instance, ClassName: class, PointName: point,
		// The column is jsonb and NOT NULL; the sync always writes the point's
		// property array. Empty is not valid json, and an empty ARRAY is what a
		// point with no properties actually has.
		PropertiesJSON: "[]",
	}), "seed scene point "+instance)
}
