package domain

import (
	"testing"

	"shingo/protocol"
)

func fp(v float64) *float64 { return &v }

func fullScene() (string, []protocol.ScenePointInfo, []protocol.SceneEdgeInfo) {
	return "rev-1",
		[]protocol.ScenePointInfo{
			{InstanceName: "PLN_ORIGIN", ClassName: "GeneralLocation", PosX: fp(0), PosY: fp(0), Dir: fp(0)},
			{InstanceName: "LM9", ClassName: "LocationMark", PosX: fp(-0.604), PosY: fp(22.449), Dir: fp(1.5708)},
		},
		[]protocol.SceneEdgeInfo{
			{From: "LM9", To: "PP224", FromX: fp(-0.604), FromY: fp(22.449), ToX: fp(0.986), ToY: fp(22.169),
				Ctrl1X: fp(-0.287), Ctrl1Y: fp(22.094), Ctrl2X: fp(0.303), Ctrl2Y: fp(22.142)},
			{From: "CP51", To: "LM54", FromX: fp(-9.338), FromY: fp(6.23), ToX: fp(-8.855), ToY: fp(6.221)},
		}
}

// TestNewSceneGeometry_AcceptsACompleteScene is the shape a full response
// takes on the Edge: every point placed, (0,0) kept, handles all-four-or-none.
func TestNewSceneGeometry_AcceptsACompleteScene(t *testing.T) {
	t.Parallel()
	rev, pts, eds := fullScene()
	g, err := NewSceneGeometry(rev, pts, eds)
	if err != nil {
		t.Fatalf("a complete scene was refused: %v", err)
	}
	if g.Revision != "rev-1" {
		t.Errorf("revision = %q", g.Revision)
	}
	o, ok := g.Points["PLN_ORIGIN"]
	if !ok {
		t.Fatal("PLN_ORIGIN not cached")
	}
	if o.X != 0 || o.Y != 0 || o.ClassName != "GeneralLocation" {
		t.Errorf("origin cached as %+v — (0,0) is a real coordinate", o)
	}
	if len(g.Edges) != 2 {
		t.Fatalf("%d edges cached, want 2", len(g.Edges))
	}
	var bez, straight *SceneEdgeGeom
	for i := range g.Edges {
		switch g.Edges[i].From {
		case "LM9":
			bez = &g.Edges[i]
		case "CP51":
			straight = &g.Edges[i]
		}
	}
	if bez == nil || bez.Handles == nil || bez.Handles[0] != -0.287 || bez.Handles[3] != 22.142 {
		t.Errorf("bezier handles lost: %+v", bez)
	}
	if straight == nil || straight.Handles != nil {
		t.Errorf("a straight segment gained handles: %+v", straight)
	}
}

// TestNewSceneGeometry_RefusesAnythingShortOfComplete is the all-or-nothing
// rule that keeps a name-only or half-read response from touching the cache.
// Each case names the response shape it stands for.
func TestNewSceneGeometry_RefusesAnythingShortOfComplete(t *testing.T) {
	t.Parallel()
	rev, pts, eds := fullScene()
	namesOnly := []protocol.ScenePointInfo{{InstanceName: "PLN_ORIGIN", ClassName: "GeneralLocation"}, {InstanceName: "LM9", ClassName: "LocationMark"}}
	edgesNamesOnly := []protocol.SceneEdgeInfo{{From: "LM9", To: "PP224"}, {From: "CP51", To: "LM54"}}
	mixed := append([]protocol.ScenePointInfo{}, pts...)
	mixed[1] = protocol.ScenePointInfo{InstanceName: "LM9", ClassName: "LocationMark"}
	halfEdge := []protocol.SceneEdgeInfo{{From: "LM9", To: "PP224", FromX: fp(-0.604), FromY: fp(22.449), ToX: fp(0.986)}}

	cases := []struct {
		name string
		rev  string
		pts  []protocol.ScenePointInfo
		eds  []protocol.SceneEdgeInfo
	}{
		{"no revision (a partial read on Core)", "", pts, eds},
		{"names only (revision matched)", rev, namesOnly, edgesNamesOnly},
		{"points without edges", rev, pts, nil},
		{"edges without points", rev, nil, eds},
		{"one point without geometry", rev, mixed, eds},
		{"an edge missing an endpoint coordinate", rev, pts, halfEdge},
		{"empty scene", rev, nil, nil},
	}
	for _, c := range cases {
		if g, err := NewSceneGeometry(c.rev, c.pts, c.eds); err == nil {
			t.Errorf("%s: accepted as complete (%d points, %d edges) — this response must leave the cache untouched", c.name, len(g.Points), len(g.Edges))
		}
	}
}

// TestNewSceneGeometry_PartialHandlesAreNotACurve: three of four handle
// coordinates describe no cubic. The segment is kept — its endpoints are
// good — as a chord, and nothing invents the fourth number.
func TestNewSceneGeometry_PartialHandlesAreNotACurve(t *testing.T) {
	t.Parallel()
	rev, pts, _ := fullScene()
	eds := []protocol.SceneEdgeInfo{{From: "LM9", To: "PP224", FromX: fp(-0.604), FromY: fp(22.449), ToX: fp(0.986), ToY: fp(22.169),
		Ctrl1X: fp(-0.287), Ctrl1Y: fp(22.094), Ctrl2X: fp(0.303)}}
	g, err := NewSceneGeometry(rev, pts, eds)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if g.Edges[0].Handles != nil {
		t.Errorf("a three-handle segment was cached as a curve: %v", *g.Edges[0].Handles)
	}
}
