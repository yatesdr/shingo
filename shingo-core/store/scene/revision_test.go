package scene

import (
	"testing"
	"time"

	"shingo/protocol"
)

// Revision is a pure function of the rows, and these pin the three properties
// the node-list short-circuit rests on: read order does not matter, a moved
// row changes it, and a point and an edge with the same id are not confused.
func TestRevision_IsAFunctionOfTheRowsOnly(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 9, 3, 1, 44, 23, 0, time.UTC)
	p1 := &Point{ID: 1, InstanceName: "PLN_001", SyncedAt: t0}
	p2 := &Point{ID: 2, InstanceName: "LM9", SyncedAt: t0}
	e1 := &Edge{ID: 1, FromName: "LM9", ToName: "PP224", SyncedAt: t0}

	a := Revision([]*Point{p1, p2}, []*Edge{e1})
	b := Revision([]*Point{p2, p1}, []*Edge{e1})
	if a != b {
		t.Errorf("read order changed the revision: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("revision %q is not a hex SHA-256", a)
	}

	// The same rows re-read later, untouched: same revision. The clock is not
	// an input.
	if c := Revision([]*Point{p1, p2}, []*Edge{e1}); c != a {
		t.Errorf("an unchanged scene produced a new revision")
	}

	// A point re-synced (synced_at bumped, nothing else) is a new scene.
	moved := &Point{ID: 2, InstanceName: "LM9", SyncedAt: t0.Add(time.Second)}
	if d := Revision([]*Point{p1, moved}, []*Edge{e1}); d == a {
		t.Error("a re-synced point left the revision unchanged")
	}

	// A row removed is a new scene, even with the count restored by an
	// insert that happens to carry an OLDER synced_at than the table's max.
	replaced := &Point{ID: 3, InstanceName: "LM10", SyncedAt: t0.Add(-time.Hour)}
	if e := Revision([]*Point{p1, replaced}, []*Edge{e1}); e == a {
		t.Error("delete+insert with the count preserved left the revision unchanged — this is the case max(synced_at)+count misses")
	}

	// Point 1 and edge 1 share an id; moving an edge into the point table (or
	// vice versa) must not collide.
	if f := Revision([]*Point{p1, p2, {ID: 9, SyncedAt: t0}}, []*Edge{e1}); f == Revision([]*Point{p1, p2}, []*Edge{e1, {ID: 9, SyncedAt: t0}}) {
		t.Error("a point and an edge with the same id and stamp hash alike")
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
