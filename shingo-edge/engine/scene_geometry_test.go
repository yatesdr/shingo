package engine

import (
	"testing"

	"shingo/protocol"
)

func fp(v float64) *float64 { return &v }

func completeScene() ([]protocol.ScenePointInfo, []protocol.SceneEdgeInfo) {
	return []protocol.ScenePointInfo{
			{InstanceName: "PLN_01", ClassName: "GeneralLocation", PosX: fp(-16.929), PosY: fp(59.549), Dir: fp(0)},
			{InstanceName: "AP143", ClassName: "ActionPoint", PosX: fp(-17.554), PosY: fp(59.061), Dir: fp(0)},
		}, []protocol.SceneEdgeInfo{
			{From: "AP143", To: "LM119", FromX: fp(-17.554), FromY: fp(59.061), ToX: fp(-17.5), ToY: fp(58)},
		}
}

// TestSceneGeometry_HotCopyFollowsTheCompleteOrNothingRule pins the engine's
// in-memory copy: a complete response replaces it and sets the revision the
// heartbeater will quote; a name-only or partial response leaves both alone.
func TestSceneGeometry_HotCopyFollowsTheCompleteOrNothingRule(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if e.SceneRevision() != "" || e.SceneGeometry() != nil {
		t.Fatal("a fresh engine must hold no revision and no geometry")
	}

	pts, eds := completeScene()
	e.SetSceneGeometry("rev-1", pts, eds)
	if got := e.SceneRevision(); got != "rev-1" {
		t.Fatalf("revision after a complete response = %q, want rev-1", got)
	}
	g := e.SceneGeometry()
	if g == nil || g.Points["PLN_01"].X != -16.929 {
		t.Fatalf("geometry not cached: %+v", g)
	}

	// The next sync matched: names only, same revision. Nothing moves.
	e.SetSceneGeometry("rev-1",
		[]protocol.ScenePointInfo{{InstanceName: "PLN_01", ClassName: "GeneralLocation"}, {InstanceName: "AP143", ClassName: "ActionPoint"}},
		[]protocol.SceneEdgeInfo{{From: "AP143", To: "LM119"}})
	if e.SceneRevision() != "rev-1" || e.SceneGeometry().Points["PLN_01"].X != -16.929 {
		t.Error("a name-only response disturbed the cached geometry")
	}

	// A half-read scene on Core: geometry on the points, nothing for edges,
	// and no revision. The cache must not take it.
	e.SetSceneGeometry("", pts, nil)
	if e.SceneRevision() != "rev-1" || len(e.SceneGeometry().Edges) != 1 {
		t.Error("a partial response replaced the cache")
	}

	// A genuinely new scene replaces everything, including the revision.
	pts2, eds2 := completeScene()
	pts2[0].PosX = fp(-16.5)
	e.SetSceneGeometry("rev-2", pts2, eds2)
	if e.SceneRevision() != "rev-2" || e.SceneGeometry().Points["PLN_01"].X != -16.5 {
		t.Errorf("a new complete response did not replace the cache: rev=%q pts=%+v", e.SceneRevision(), e.SceneGeometry().Points)
	}
}

// TestSceneGeometry_NameSetStillComesFromSetSceneGraph: the validator's name
// set keeps its own path and its own "nil means could not look" contract; the
// geometry cache is a second consumer of the same slices, not a replacement.
func TestSceneGeometry_NameSetStillComesFromSetSceneGraph(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	pts, eds := completeScene()
	e.SetSceneGeometry("rev-1", pts, eds)
	if e.ScenePointNames() != nil {
		t.Error("SetSceneGeometry populated the validator's name set — that is SetSceneGraph's job")
	}
}
