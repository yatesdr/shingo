//go:build docker

package scene_test

import (
	"database/sql"
	"strconv"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/scene"
)

func f64(v float64) *float64 { return &v }

// handles renders an edge's four nullable coordinates as coordinates. %v on a
// *float64 prints an address, which turns a real failure into a hex dump —
// and this is a test whose whole subject is which VALUES came back.
func handles(e *scene.Edge) string {
	f := func(p *float64) string {
		if p == nil {
			return "NULL"
		}
		return strconv.FormatFloat(*p, 'g', -1, 64)
	}
	return "(" + f(e.Ctrl1X) + "," + f(e.Ctrl1Y) + ")/(" + f(e.Ctrl2X) + "," + f(e.Ctrl2Y) + ")"
}

// TestCoverage_SceneEdge_ControlHandlesRoundTrip is the READER for migration
// 62's four columns. A column with a migration, a struct field and every writer
// but nothing that reads it back has passed a full gate on this repo before
// (closed_by), so the assertion here is on the VALUES that come out of
// Postgres, not on the absence of an error from the INSERT.
//
// It also pins the two things NULL has to mean. A straight aisle stores four
// NULLs and reads back as not-Curved, which is what makes the renderer's
// straight-vs-cubic decision safe; and re-syncing a scene that has since been
// straightened must CLEAR the stored handles, which only the UPDATE half of
// the upsert can do. An upsert whose SET clause is exercised only by its own
// INSERT is the same family of bug as the write-only column.
func TestCoverage_SceneEdge_ControlHandlesRoundTrip(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// LM10-LM113 at Springfield: a DegenerateBezier that really does bow,
	// 1.30 m off its own chord.
	curved := &scene.Edge{
		AreaName: "SPR", InstanceName: "LM10-LM113", ClassName: "DegenerateBezier",
		FromName: "LM10", ToName: "LM113",
		FromX: -0.254, FromY: 33.554, ToX: -6.572, ToY: 36.951,
		Ctrl1X: f64(-0.198), Ctrl1Y: f64(36.401),
		Ctrl2X: f64(-5.065), Ctrl2Y: f64(36.951),
	}
	// LM100-AP102: straight, and the scene says so with a zero pair that never
	// gets this far.
	straight := &scene.Edge{
		AreaName: "SPR", InstanceName: "LM100-AP102", ClassName: "StraightPath",
		FromName: "LM100", ToName: "AP102",
		FromX: -0.544, FromY: 11.787, ToX: 0.886, ToY: 11.807,
	}
	for _, e := range []*scene.Edge{curved, straight} {
		if err := scene.UpsertEdge(db.DB, e); err != nil {
			t.Fatalf("UpsertEdge %s: %v", e.InstanceName, err)
		}
	}

	got := edgesByName(t, db.DB)

	c := got["LM10-LM113"]
	if c == nil {
		t.Fatal("curved edge missing after insert")
	}
	if !c.Curved() {
		t.Fatalf("curved edge read back without handles: %+v", c)
	}
	if *c.Ctrl1X != -0.198 || *c.Ctrl1Y != 36.401 || *c.Ctrl2X != -5.065 || *c.Ctrl2Y != 36.951 {
		t.Errorf("handles read back as (%v,%v)/(%v,%v), want (-0.198,36.401)/(-5.065,36.951)",
			*c.Ctrl1X, *c.Ctrl1Y, *c.Ctrl2X, *c.Ctrl2Y)
	}

	s := got["LM100-AP102"]
	if s == nil {
		t.Fatal("straight edge missing after insert")
	}
	if s.Curved() {
		t.Errorf("straight aisle read back as curved: %s", handles(s))
	}
	if s.Ctrl1X != nil || s.Ctrl1Y != nil || s.Ctrl2X != nil || s.Ctrl2Y != nil {
		t.Errorf("straight aisle stored a handle coordinate: %s", handles(s))
	}

	// THE UPDATE HALF. Somebody straightens LM10-LM113 in RoboShop; the next
	// sync re-upserts it with no handles. If the SET clause omitted them the
	// map would keep drawing yesterday's bend forever, with nothing anywhere
	// saying it was stale.
	straightened := *curved
	straightened.ClassName = "StraightPath"
	straightened.Ctrl1X, straightened.Ctrl1Y = nil, nil
	straightened.Ctrl2X, straightened.Ctrl2Y = nil, nil
	if err := scene.UpsertEdge(db.DB, &straightened); err != nil {
		t.Fatalf("UpsertEdge straightened: %v", err)
	}
	after := edgesByName(t, db.DB)["LM10-LM113"]
	if after == nil {
		t.Fatal("edge vanished on update")
	}
	if after.Curved() {
		t.Errorf("straightening the lane left the old bend stored: %s", handles(after))
	}
	if after.ClassName != "StraightPath" {
		t.Errorf("class after update = %q, want StraightPath", after.ClassName)
	}

	// And the reverse: a lane that gains a bend picks it up on the same path.
	rebent := straightened
	rebent.ClassName = "BezierPath"
	rebent.Ctrl1X, rebent.Ctrl1Y = f64(-0.287), f64(22.094)
	rebent.Ctrl2X, rebent.Ctrl2Y = f64(0.303), f64(22.142)
	if err := scene.UpsertEdge(db.DB, &rebent); err != nil {
		t.Fatalf("UpsertEdge rebent: %v", err)
	}
	final := edgesByName(t, db.DB)["LM10-LM113"]
	if final == nil || !final.Curved() {
		t.Fatalf("a lane that gained a bend did not store it: %+v", final)
	}
	if *final.Ctrl1X != -0.287 || *final.Ctrl2Y != 22.142 {
		t.Errorf("new handles = (%v,..)/(..,%v), want (-0.287,..)/(..,22.142)",
			*final.Ctrl1X, *final.Ctrl2Y)
	}
}

func edgesByName(t *testing.T, db *sql.DB) map[string]*scene.Edge {
	t.Helper()
	all, err := scene.ListEdges(db)
	if err != nil {
		t.Fatalf("ListEdges: %v", err)
	}
	out := make(map[string]*scene.Edge, len(all))
	for _, e := range all {
		out[e.InstanceName] = e
	}
	return out
}
