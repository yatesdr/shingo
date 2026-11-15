package engine

import (
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
