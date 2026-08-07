//go:build docker

package scene_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/scene"
)

// ReplaceArea against a real Postgres: the swap is atomic, and a failure inside
// it leaves the previous scene intact.
//
// This is the one property that only a real database can show. The scenesync
// fake models the rollback, which keeps the CALLER honest; it cannot show that
// the transaction is actually there. A ReplaceArea that ran its statements on
// the bare connection would pass every test in scenesync and fail here.

func TestReplaceArea_SwapsPointsAndEdgesTogether(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// A scene that is already there.
	if err := scene.ReplaceArea(db.DB, "AREA-R1",
		[]*scene.Point{{AreaName: "AREA-R1", InstanceName: "old-pt", ClassName: "AP", PropertiesJSON: "{}"}},
		[]*scene.Edge{{AreaName: "AREA-R1", InstanceName: "old-edge", FromName: "A", ToName: "B"}},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Replaced wholesale.
	if err := scene.ReplaceArea(db.DB, "AREA-R1",
		[]*scene.Point{{AreaName: "AREA-R1", InstanceName: "new-pt", ClassName: "AP", PropertiesJSON: "{}"}},
		[]*scene.Edge{{AreaName: "AREA-R1", InstanceName: "new-edge", FromName: "C", ToName: "D"}},
	); err != nil {
		t.Fatalf("replace: %v", err)
	}

	pts, err := scene.ListByArea(db.DB, "AREA-R1")
	if err != nil {
		t.Fatalf("list points: %v", err)
	}
	if len(pts) != 1 || pts[0].InstanceName != "new-pt" {
		t.Errorf("points = %+v, want exactly [new-pt] — the old set should be gone", pts)
	}

	edges, err := scene.ListEdges(db.DB)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	var got []string
	for _, e := range edges {
		if e.AreaName == "AREA-R1" {
			got = append(got, e.InstanceName)
		}
	}
	if len(got) != 1 || got[0] != "new-edge" {
		t.Errorf("edges = %v, want exactly [new-edge]", got)
	}
}

// THE ROLLBACK IS THE POINT, and the failure used here is the one that actually
// happens.
//
// The second point carries a properties_json the column cannot parse. The DELETE
// and the first INSERT have already run inside the transaction when it aborts —
// so without a transaction the delete would have committed and the area would be
// left EMPTY, or holding one row of a set that should have had two. With one, the
// area still holds exactly what it held before.
//
// (A duplicate instance name does NOT work as the failure here, which is worth
// recording: both upserts are ON CONFLICT DO UPDATE, so a repeat is an update,
// not a violation. The first draft of this test used one and passed for the
// wrong reason.)
//
// The empty-area state is precisely what the confidence roll-up reads as a
// plant-wide loss of a zone's samples, so this assertion stands between a failed
// sync and a silently short day.
func TestReplaceArea_FailureLeavesThePreviousSceneIntact(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if err := scene.ReplaceArea(db.DB, "AREA-R2",
		[]*scene.Point{
			{AreaName: "AREA-R2", InstanceName: "keep-1", ClassName: "AP", PropertiesJSON: "{}"},
			{AreaName: "AREA-R2", InstanceName: "keep-2", ClassName: "AP", PropertiesJSON: "{}"},
		},
		[]*scene.Edge{{AreaName: "AREA-R2", InstanceName: "keep-edge", FromName: "A", ToName: "B"}},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := scene.ReplaceArea(db.DB, "AREA-R2",
		[]*scene.Point{
			{AreaName: "AREA-R2", InstanceName: "new-1", ClassName: "AP", PropertiesJSON: "{}"},
			{AreaName: "AREA-R2", InstanceName: "new-2", ClassName: "AP", PropertiesJSON: "not json"},
		},
		nil,
	)
	if err == nil {
		t.Fatal("expected the unparseable properties_json to fail the replace")
	}

	pts, lerr := scene.ListByArea(db.DB, "AREA-R2")
	if lerr != nil {
		t.Fatalf("list points: %v", lerr)
	}
	if len(pts) != 2 {
		t.Errorf("after a failed replace the area holds %d point(s), want the "+
			"original 2 — a committed DELETE with no matching INSERT is the gap "+
			"the roll-up reads as orphans: %+v", len(pts), pts)
	}

	edges, lerr := scene.ListEdges(db.DB)
	if lerr != nil {
		t.Fatalf("list edges: %v", lerr)
	}
	var kept int
	for _, e := range edges {
		if e.AreaName == "AREA-R2" {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("after a failed replace the area holds %d edge(s), want the original 1", kept)
	}
}

// Replacing with nothing is a legitimate operation — it is how the stale-area
// sweep retires an area RDS no longer reports — and it takes the same
// transaction, so points and edges go together.
func TestReplaceArea_EmptyReplaceSweepsTheWholeArea(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if err := scene.ReplaceArea(db.DB, "AREA-R3",
		[]*scene.Point{{AreaName: "AREA-R3", InstanceName: "ghost-pt", ClassName: "AP", PropertiesJSON: "{}"}},
		[]*scene.Edge{{AreaName: "AREA-R3", InstanceName: "ghost-edge", FromName: "A", ToName: "B"}},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := scene.ReplaceArea(db.DB, "AREA-R3", nil, nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	pts, err := scene.ListByArea(db.DB, "AREA-R3")
	if err != nil {
		t.Fatalf("list points: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("points = %+v, want none", pts)
	}
	edges, err := scene.ListEdges(db.DB)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	for _, e := range edges {
		if e.AreaName == "AREA-R3" {
			t.Errorf("edge %q survived the sweep", e.InstanceName)
		}
	}
}
