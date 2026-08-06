package scenesync

import (
	"testing"

	"shingocore/fleet"
	"shingocore/store/scene"
)

func namedEdge(instance, from, to string, fx, fy, tx, ty float64) fleet.SceneEdge {
	return fleet.SceneEdge{
		InstanceName: instance, ClassName: "StraightPath",
		FromName: from, ToName: to,
		FromX: fx, FromY: fy, ToX: tx, ToY: ty,
	}
}

// THE ORDERING TEST. The diff has to run while the previous geometry still
// exists.
//
// SyncScenePoints mirrors the fleet by deleting each area's edges and
// re-inserting them. A diff taken after that has nothing to compare against,
// so max_vertex_delta_m would be uncomputable and every sync would look like
// a lane appearing for the first time. The fake records how many edges were
// still stored at the moment ApplyLaneVersions was called, which is the only
// way to assert an ordering rather than an outcome.
func TestSyncScenePoints_DiffsBeforeItDestroys(t *testing.T) {
	t.Parallel()
	db := newFakeStore()
	// Two stored edges — one lane, both directions — that the sync is about
	// to delete and rewrite.
	_ = db.UpsertSceneEdge(&scene.Edge{AreaName: "SPR", InstanceName: "LM1-LM2",
		FromName: "LM1", ToName: "LM2", FromX: 0, FromY: 0, ToX: 5, ToY: 0})
	_ = db.UpsertSceneEdge(&scene.Edge{AreaName: "SPR", InstanceName: "LM2-LM1",
		FromName: "LM2", ToName: "LM1", FromX: 5, FromY: 0, ToX: 0, ToY: 0})

	areas := []fleet.SceneArea{{Name: "SPR", Edges: []fleet.SceneEdge{
		namedEdge("LM1-LM2", "LM1", "LM2", 0, 0, 5.5, 0),
		namedEdge("LM2-LM1", "LM2", "LM1", 5.5, 0, 0, 0),
	}}}

	SyncScenePoints(db, noopLog, areas, "scene-md5-abc", nil)

	if len(db.laneDiffs) != 1 {
		t.Fatalf("expected exactly one lane diff, got %d", len(db.laneDiffs))
	}
	got := db.laneDiffs[0]
	if got.EdgesAtCallTime != 2 {
		t.Errorf("the diff ran with %d stored edges — it must run BEFORE the "+
			"delete, while the previous geometry still exists, or nothing can "+
			"measure how far anything moved", got.EdgesAtCallTime)
	}
	if got.GateHash != "scene-md5-abc" {
		t.Errorf("gate hash = %q, want the scene_md5 that moved — without it a "+
			"Core restart is indistinguishable from a map edit", got.GateHash)
	}
	if len(got.Lanes) != 1 {
		t.Fatalf("two directed rows are ONE lane; got %d", len(got.Lanes))
	}
	if got.Lanes[0].Lane != "LM1-LM2" {
		t.Errorf("lane = %q, want the sorted pair", got.Lanes[0].Lane)
	}
	if !got.Lanes[0].Version.TwinsAgree {
		t.Errorf("a mirrored pair should agree: %s", got.Lanes[0].Version.Disagreement)
	}
	if n := len(got.Lanes[0].Shape); n != 2 {
		t.Errorf("shape has %d vertices, want 2 — without it no later version "+
			"can measure movement from this one", n)
	}
}

// An edge the fleet publishes without endpoint names is REFUSED, not stored.
//
// scene_edges permits it — the columns are NOT NULL with an empty default —
// and the result is a row that can never be aggregated or versioned, whose
// samples the roll-up quarantines forever. Refusing it at the source is the
// fix that makes the quarantine unnecessary for everything written from here
// on.
func TestSyncScenePoints_RefusesAnEdgeItCannotName(t *testing.T) {
	t.Parallel()
	db := newFakeStore()
	areas := []fleet.SceneArea{{Name: "SPR", Edges: []fleet.SceneEdge{
		namedEdge("LM1-LM2", "LM1", "LM2", 0, 0, 5, 0),
		// Both names missing, and the half-named case the adapter lets past.
		{InstanceName: "NAMELESS", ClassName: "StraightPath"},
		{InstanceName: "HALF", ClassName: "StraightPath", FromName: "LM9"},
	}}}

	SyncScenePoints(db, noopLog, areas, "", nil)

	if _, ok := db.edges["SPR/LM1-LM2"]; !ok {
		t.Error("the properly named edge must still be stored")
	}
	for _, bad := range []string{"SPR/NAMELESS", "SPR/HALF"} {
		if _, ok := db.edges[bad]; ok {
			t.Errorf("%s was stored — an edge with no lane key can never be "+
				"aggregated or versioned, and every sample landing on it is lost "+
				"to the quarantine", bad)
		}
	}
}

// A lane whose two directions stop mirroring is flagged rather than silently
// reduced to whichever twin the loop reached first.
func TestSyncScenePoints_DivergentTwinsReachTheDiff(t *testing.T) {
	t.Parallel()
	db := newFakeStore()
	areas := []fleet.SceneArea{{Name: "SPR", Edges: []fleet.SceneEdge{
		namedEdge("LM1-LM2", "LM1", "LM2", 0, 0, 5, 0),
		// The reverse direction drawn to a different endpoint.
		namedEdge("LM2-LM1", "LM2", "LM1", 5, 3, 0, 0),
	}}}

	SyncScenePoints(db, noopLog, areas, "", nil)

	if len(db.laneDiffs) != 1 || len(db.laneDiffs[0].Lanes) != 1 {
		t.Fatalf("expected one lane in one diff, got %+v", db.laneDiffs)
	}
	v := db.laneDiffs[0].Lanes[0].Version
	if v.TwinsAgree {
		t.Error("twins that do not mirror must not be recorded as agreeing")
	}
	if v.Disagreement == "" {
		t.Error("no reason recorded for the disagreement")
	}
}
