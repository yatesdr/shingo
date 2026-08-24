package engine

import (
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
)

// TestSceneGraph_CachesWhatTheSyncDelivers pins the two properties the
// key-route work depends on: the map's point set is a NAME set the validator
// can ask, and "nothing received" is distinguishable from "the map is empty".
func TestSceneGraph_CachesWhatTheSyncDelivers(t *testing.T) {
	t.Parallel()
	e := &Engine{}

	// Before any sync: nil, which every consumer reads as "could not look".
	if got := e.ScenePointNames(); got != nil {
		t.Errorf("a fresh Edge must answer nil, not an empty map — an empty map reads as "+
			"'the plant has no points', which would refuse every key route; got %v", got)
	}

	e.SetSceneGraph([]protocol.ScenePointInfo{
		{InstanceName: "WP_AISLE_N", ClassName: "LM"},
		{InstanceName: "SMN.001", ClassName: "AP"},
		{InstanceName: "", ClassName: "LM"}, // unnamed points cannot be routed to
	}, nil)

	names := e.ScenePointNames()
	if !names["WP_AISLE_N"] {
		t.Error("a plain waypoint must be in the point set — it is the whole reason this exists")
	}
	if !names["SMN.001"] {
		t.Error("an action point is a map point too")
	}
	if names[""] {
		t.Error("a blank instance name is not a routable point")
	}
	if len(names) != 2 {
		t.Errorf("point set = %v, want exactly the two named points", names)
	}
}

// TestSceneGraph_AdjacencyIsUndirected pins the picker's input. A scene edge is
// a drivable segment and the fleet routes over it in whichever direction the
// plan needs; a one-way reading would hide half the neighbours of every point
// and offer a picker that omits the obvious answer.
func TestSceneGraph_AdjacencyIsUndirected(t *testing.T) {
	t.Parallel()
	e := &Engine{}

	if got := e.SceneAdjacency(); got != nil {
		t.Errorf("no edges means nil, not an empty map; got %v", got)
	}

	e.SetSceneGraph(nil, []protocol.SceneEdgeInfo{
		{From: "AP_PRESS", To: "WP_AISLE_N"},
		{From: "WP_AISLE_N", To: "WP_AISLE_S"},
		{From: "AP_PRESS", To: "WP_AISLE_N"}, // duplicate segment
		{From: "SELF", To: "SELF"},           // degenerate
		{From: "", To: "WP_AISLE_S"},         // half-named
	})

	adj := e.SceneAdjacency()
	for from, want := range map[string][]string{
		"AP_PRESS":   {"WP_AISLE_N"},
		"WP_AISLE_N": {"AP_PRESS", "WP_AISLE_S"},
		"WP_AISLE_S": {"WP_AISLE_N"},
	} {
		got := append([]string(nil), adj[from]...)
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("neighbours of %s = %v, want %v", from, got, want)
		}
	}
	if _, ok := adj["SELF"]; ok {
		t.Error("a segment from a point to itself joins nothing")
	}
	if _, ok := adj[""]; ok {
		t.Error("a segment with an unnamed end joins nothing")
	}
}
