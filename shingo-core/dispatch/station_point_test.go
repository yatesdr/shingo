package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
)

// TestStationPoint pins the wire's numbering of a wait: its ordinal among the
// plan's STATION waits, which the Edge counts the same way because Core's
// spliced lane waits are not in the Edge's copy of the plan.
func TestStationPoint(t *testing.T) {
	t.Parallel()
	w := func(kind string) resolvedStep { return resolvedStep{Action: protocol.ActionWait, WaitKind: kind} }
	p := resolvedStep{Action: protocol.ActionPickup, Node: "N"}
	plan := []resolvedStep{w(WaitKindLane), p, w(WaitKindStation), p, w(""), p, w(WaitKindLane), p, w(WaitKindStation)}
	for _, c := range []struct {
		waitIndex int
		want      string
	}{
		{0, "lane"}, {1, "station:0"}, {2, "station:1"}, {3, "lane"}, {4, "station:2"}, {5, "none"}, {-1, "none"},
	} {
		at, kind, ok := stationPoint(plan, c.waitIndex)
		got := "none"
		switch {
		case ok && at != nil:
			got = fmt.Sprintf("%s:%d", kind, *at)
		case ok:
			got = kind
		}
		if got != c.want {
			t.Errorf("stationPoint(wait_index %d) = %s, want %s", c.waitIndex, got, c.want)
		}
	}
}
