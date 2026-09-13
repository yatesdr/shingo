package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// scene_geometry_test.go — the geometry half of the scene slices on the node
// list, and the revision that lets Core leave it off.
//
// The wire carries coordinates as POINTERS, and every test here is about the
// one property that forces that: (0,0) is a real place on a plant map. A bare
// float64 with omitempty drops a zero, so a point at the origin would arrive
// looking exactly like a point whose geometry was never sent — and the Edge
// cache's "complete or nothing" rule would then refuse a perfectly good scene.

func fptr(v float64) *float64 { return &v }

func TestScenePointInfo_ZeroCoordinateSurvivesTheWire(t *testing.T) {
	t.Parallel()
	in := ScenePointInfo{InstanceName: "ORIGIN", ClassName: "GeneralLocation", PosX: fptr(0), PosY: fptr(0), Dir: fptr(0)}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"pos_x":0`, `"pos_y":0`, `"dir":0`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire form %s is missing %s — a zero coordinate has been dropped as if it were absent", raw, key)
		}
	}
	var out ScenePointInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PosX == nil || out.PosY == nil || out.Dir == nil {
		t.Fatalf("decoded point lost its geometry: %+v", out)
	}
	if *out.PosX != 0 || *out.PosY != 0 || *out.Dir != 0 {
		t.Errorf("decoded (%v,%v,%v), want (0,0,0)", *out.PosX, *out.PosY, *out.Dir)
	}
}

func TestScenePointInfo_NameOnlyOmitsTheGeometryKeys(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(ScenePointInfo{InstanceName: "LM1", ClassName: "LocationMark"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); got != `{"instance_name":"LM1","class_name":"LocationMark"}` {
		t.Errorf("name-only point = %s; the pre-geometry wire form must be byte-identical so an older Edge sees nothing new", got)
	}
	var out ScenePointInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PosX != nil || out.PosY != nil || out.Dir != nil {
		t.Errorf("a name-only point decoded with geometry: %+v — absence must stay nil, never zero", out)
	}
}

func TestSceneEdgeInfo_StraightSegmentCarriesNoHandles(t *testing.T) {
	t.Parallel()
	straight := SceneEdgeInfo{From: "LM54", To: "CP51", FromX: fptr(-8.855), FromY: fptr(6.221), ToX: fptr(-9.338), ToY: fptr(6.23)}
	raw, err := json.Marshal(straight)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "ctrl") {
		t.Errorf("a straight segment put a handle on the wire: %s — a NULL handle column is nil, and nil is omitted, never invented", raw)
	}
	var out SceneEdgeInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Ctrl1X != nil || out.Ctrl1Y != nil || out.Ctrl2X != nil || out.Ctrl2Y != nil {
		t.Errorf("straight segment decoded with handles: %+v", out)
	}
	if out.FromX == nil || *out.FromX != -8.855 || out.ToY == nil || *out.ToY != 6.23 {
		t.Errorf("endpoints did not round-trip: %+v", out)
	}
}

func TestSceneEdgeInfo_BezierCarriesAllFourHandles(t *testing.T) {
	t.Parallel()
	bez := SceneEdgeInfo{From: "AP123", To: "LM135",
		FromX: fptr(-35.459), FromY: fptr(57.966), ToX: fptr(-38.448), ToY: fptr(58.156),
		Ctrl1X: fptr(-36.455), Ctrl1Y: fptr(58.029), Ctrl2X: fptr(-37.452), Ctrl2Y: fptr(58.093)}
	raw, err := json.Marshal(bez)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out SceneEdgeInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for name, got := range map[string]*float64{"ctrl1_x": out.Ctrl1X, "ctrl1_y": out.Ctrl1Y, "ctrl2_x": out.Ctrl2X, "ctrl2_y": out.Ctrl2Y} {
		if got == nil {
			t.Errorf("%s lost on the wire: %s", name, raw)
		}
	}
	if out.Ctrl1X != nil && *out.Ctrl1X != -36.455 {
		t.Errorf("ctrl1_x = %v, want -36.455", *out.Ctrl1X)
	}
}

// TestNodeListRequest_SceneRevisionIsAdditive pins both directions of the
// compatibility promise: an Edge that has no revision to send produces the
// exact `{}` the pre-geometry binary sent, and an older Core decoding a request
// that carries one simply never reads it.
func TestNodeListRequest_SceneRevisionIsAdditive(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(NodeListRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{}` {
		t.Errorf("empty request = %s, want {} — an older Core must see the request it always saw", raw)
	}
	raw, err = json.Marshal(NodeListRequest{SceneRevision: "abc123"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"scene_revision":"abc123"}` {
		t.Errorf("request = %s, want the revision under scene_revision", raw)
	}
	var out NodeListRequest
	if err := json.Unmarshal([]byte(`{}`), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.SceneRevision != "" {
		t.Errorf("an older Edge's {} decoded with a revision %q", out.SceneRevision)
	}
}

func TestNodeListResponse_SceneRevisionOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(NodeListResponse{Nodes: []NodeInfo{{Name: "A", NodeType: "PLN"}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "scene_revision") {
		t.Errorf("a response with no scene put an empty revision on the wire: %s", raw)
	}
	raw, err = json.Marshal(NodeListResponse{SceneRevision: "r1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"scene_revision":"r1"`) {
		t.Errorf("revision missing from %s", raw)
	}
}
