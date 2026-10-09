package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
)

// TestSceneGeometry_NoSceneIsHeld: Core's SceneRevisionNone clears the
// geometry, the heartbeat quotes it back (so the scene feed converges), the
// cell picture falls back to the schematic, a repeat writes nothing and bumps
// nothing, and a restart still holds it. A later real scene replaces it.
func TestSceneGeometry_NoSceneIsHeld(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	pts, eds := completeScene()
	e := &Engine{db: db}
	e.SetSceneGeometry("rev-1", pts, eds)

	e.SetSceneGeometry(protocol.SceneRevisionNone, nil, nil)
	if e.SceneRevision() != protocol.SceneRevisionNone || e.SceneGeometry() != nil {
		t.Fatalf("after Core's no-scene: revision %q, geometry %+v; want %q and nil",
			e.SceneRevision(), e.SceneGeometry(), protocol.SceneRevisionNone)
	}
	if got := e.FeedDigests()[protocol.FeedScene]; got != protocol.SceneRevisionNone {
		t.Errorf("heartbeat quotes scene %q, want %q", got, protocol.SceneRevisionNone)
	}
	gen := e.PlantGeneration()
	e.SetSceneGeometry(protocol.SceneRevisionNone, nil, nil)
	if e.PlantGeneration() != gen {
		t.Error("a repeated no-scene bumped the plant generation")
	}
	stored, err := db.LoadSceneGeometry()
	testutil.MustNoErr(t, err, "load")
	if stored == nil || stored.Revision != protocol.SceneRevisionNone || len(stored.Points) != 0 || len(stored.Edges) != 0 {
		t.Fatalf("store holds %+v, want the no-scene revision and no rows", stored)
	}

	second := &Engine{db: db}
	second.loadSceneGeometry()
	if second.SceneRevision() != protocol.SceneRevisionNone || second.SceneGeometry() != nil {
		t.Errorf("after restart: revision %q, geometry %+v; want %q and nil", second.SceneRevision(), second.SceneGeometry(), protocol.SceneRevisionNone)
	}

	second.SetSceneGeometry("rev-2", pts, eds)
	if second.SceneRevision() != "rev-2" || second.SceneGeometry() == nil {
		t.Errorf("a new map after no scene: revision %q, geometry nil %v", second.SceneRevision(), second.SceneGeometry() == nil)
	}
}

// TestSceneGeometry_CacheSurvivesAnEngineRestart is the durable half: a
// complete response is written through to the store, a fresh engine on the
// same database picks it up at boot, and a name-only response — the ordinary
// tick when the revision matches — writes nothing, which is read back through
// the store to prove it rather than inferred from the hot copy.
func TestSceneGeometry_CacheSurvivesAnEngineRestart(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	pts, eds := completeScene()

	first := &Engine{db: db}
	first.loadSceneGeometry()
	if first.SceneRevision() != "" {
		t.Fatalf("a fresh database yielded revision %q at boot", first.SceneRevision())
	}
	first.SetSceneGeometry("rev-1", pts, eds)

	// The name-only response the next tick brings. Nothing on disk moves.
	first.SetSceneGeometry("rev-1",
		[]protocol.ScenePointInfo{{InstanceName: "PLN_01", ClassName: "GeneralLocation"}},
		[]protocol.SceneEdgeInfo{{From: "AP143", To: "LM119"}})

	stored, err := db.LoadSceneGeometry()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored == nil || stored.Revision != "rev-1" || stored.Points["PLN_01"].X != -16.929 {
		t.Fatalf("store holds %+v after a complete then a name-only response, want rev-1 with PLN_01 placed", stored)
	}

	// "Restart": a new engine over the same file, before any sync.
	second := &Engine{db: db}
	second.loadSceneGeometry()
	if second.SceneRevision() != "rev-1" {
		t.Errorf("after restart the heartbeater would quote %q, want rev-1 — Core would resend the whole map on every boot", second.SceneRevision())
	}
	if g := second.SceneGeometry(); g == nil || g.Points["PLN_01"].Y != 59.549 || len(g.Edges) != 1 {
		t.Errorf("after restart the hot copy is %+v", g)
	}

	// A partial response after the restart still leaves disk alone.
	second.SetSceneGeometry("", pts, nil)
	again, err := db.LoadSceneGeometry()
	testutil.MustNoErr(t, err, "db.LoadSceneGeometry")
	if again == nil || again.Revision != "rev-1" {
		t.Errorf("a partial response reached the store: %+v", again)
	}
}
