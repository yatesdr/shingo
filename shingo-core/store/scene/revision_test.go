package scene

import (
	"testing"
	"time"

	"shingo/protocol"
)

// Revision is a pure function of what the rows say, and these pin the
// properties the node-list short-circuit and the scene feed rest on: read
// order does not matter, the same scene written again (new ids, new
// synced_at) is the same revision, a moved, added or removed row is a new one,
// and no two different scenes are confused by how their rows are cut into
// fields or tables.
func TestRevision_IsAFunctionOfTheContentOnly(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 9, 3, 1, 44, 23, 0, time.UTC)
	p1 := &Point{ID: 1, AreaName: "A", InstanceName: "PLN_001", ClassName: "GeneralLocation", PosX: 1.5, PosY: -2, SyncedAt: t0}
	p2 := &Point{ID: 2, AreaName: "A", InstanceName: "LM9", ClassName: "LocationMark", PosX: -0.604, PosY: 22.449, Dir: 1.5708, SyncedAt: t0}
	e1 := &Edge{ID: 1, AreaName: "A", InstanceName: "LM9-PP224", FromName: "LM9", ToName: "PP224", FromX: -0.604, ToX: 0.986, SyncedAt: t0}

	a := Revision([]*Point{p1, p2}, []*Edge{e1})
	b := Revision([]*Point{p2, p1}, []*Edge{e1})
	if a != b {
		t.Errorf("read order changed the revision: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("revision %q is not a hex SHA-256", a)
	}
	if c := Revision([]*Point{p1, p2}, []*Edge{e1}); c != a {
		t.Errorf("an unchanged scene produced a new revision")
	}

	// The same scene synced again: ReplaceArea deletes the area and inserts it,
	// so every row has a new id and a new synced_at, and nothing else moved.
	resynced := func(p Point) *Point { p.ID += 100; p.SyncedAt = t0.Add(time.Hour); return &p }
	e1again := *e1
	e1again.ID, e1again.SyncedAt = 7, t0.Add(time.Hour)
	if d := Revision([]*Point{resynced(*p1), resynced(*p2)}, []*Edge{&e1again}); d != a {
		t.Errorf("the same scene synced again has a new revision %s (was %s)", d, a)
	}

	// A point moved a millimetre is a new scene.
	moved := *p2
	moved.PosX += 0.001
	if d := Revision([]*Point{p1, &moved}, []*Edge{e1}); d == a {
		t.Error("a moved point left the revision unchanged")
	}
	// A row replaced by another, the count kept, is a new scene.
	replaced := &Point{ID: 3, AreaName: "A", InstanceName: "LM10", ClassName: "LocationMark", SyncedAt: t0.Add(-time.Hour)}
	if d := Revision([]*Point{p1, replaced}, []*Edge{e1}); d == a {
		t.Error("a row replaced, the count kept, left the revision unchanged")
	}
	// A handle at the origin is not a straight segment's missing handle.
	zero := 0.0
	bowed := *e1
	bowed.Ctrl1X, bowed.Ctrl1Y, bowed.Ctrl2X, bowed.Ctrl2Y = &zero, &zero, &zero, &zero
	if d := Revision([]*Point{p1, p2}, []*Edge{&bowed}); d == a {
		t.Error("handles at (0,0) hash like no handles")
	}

	// A point and an edge saying the same names do not collide, nor do two
	// rows that split the same text across different fields.
	if f := Revision([]*Point{p1, p2, {AreaName: "A", InstanceName: "X"}}, []*Edge{e1}); f == Revision([]*Point{p1, p2}, []*Edge{e1, {AreaName: "A", InstanceName: "X"}}) {
		t.Error("a point and an edge with the same names hash alike")
	}
	split := func(class, label string) string {
		return Revision([]*Point{p1, {AreaName: "A", InstanceName: "LM9", ClassName: class, Label: label}}, []*Edge{e1})
	}
	if split("A B", "C") == split("A", "B C") {
		t.Error("two points that cut the same text into different fields hash alike")
	}
}

// A scene the Edge cannot cache — no points, no edges, or one without the
// other — is protocol.SceneRevisionNone, which the Edge holds as "no scene".
// Found by the versioned-feeds proof: on the demo plant (no scene) Core's
// revision was the SHA-256 of nothing, the Edge could never hold it, and Core
// resent the scene every two minutes and flagged both stations "not
// converging"; a deleted map did the same to an Edge holding the old one.
func TestRevision_NoneForASceneTheEdgeCannotCache(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 10, 7, 42, 0, 0, time.UTC)
	p := &Point{ID: 1, InstanceName: "LM9", SyncedAt: t0}
	e := &Edge{ID: 1, FromName: "LM9", ToName: "PP224", SyncedAt: t0}
	for name, got := range map[string]string{
		"empty":       Revision(nil, nil),
		"points only": Revision([]*Point{p}, nil),
		"edges only":  Revision(nil, []*Edge{e}),
	} {
		if got != protocol.SceneRevisionNone {
			t.Errorf("%s: revision %q, want %q", name, got, protocol.SceneRevisionNone)
		}
	}
	if r := Revision([]*Point{p}, []*Edge{e}); r == "" || r == protocol.SceneRevisionNone {
		t.Errorf("a scene with points and edges has revision %q", r)
	}
}
