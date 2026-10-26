package domain

import (
	"strings"
	"testing"

	"shingo/protocol"
)

// routeText draws stops the way a person reads them: "→" between stops, an
// unassigned end as "—", the current stop in brackets.
func routeText(stops []RouteStop) string {
	parts := make([]string, len(stops))
	for i, s := range stops {
		p := s.Node
		if s.Unassigned {
			p = "—"
		}
		if s.Current {
			p = "[" + p + "]"
		}
		parts[i] = p
	}
	return strings.Join(parts, " → ")
}

// The press-index R1 of Springfield Press 6 (2026-10-21), as the Edge builds it
// (material_orders.go BuildTwoRobotPressIndexSwapSteps, unflipped).
const routeR1Steps = `[{"action":"wait","node":"PLN_012","wait_kind":"station"},{"action":"pickup","node":"PLN_012"},` +
	`{"action":"dropoff","node":"ULN_002"},{"action":"pickup","node":"SMN_BUF_100","empty":true},{"action":"dropoff","node":"PLN_008"}]`

// A single_robot swap's nine steps (BuildSingleSwapSteps).
const routeSingleSteps = `[{"action":"pickup","node":"SMN_01"},{"action":"dropoff","node":"IN_STG"},` +
	`{"action":"wait","node":"ALN_001"},{"action":"pickup","node":"ALN_001"},{"action":"dropoff","node":"OUT_STG"},` +
	`{"action":"pickup","node":"IN_STG"},{"action":"dropoff","node":"ALN_001"},{"action":"pickup","node":"OUT_STG"},` +
	`{"action":"dropoff","node":"ULN_001"}]`

func TestRouteStops(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		order   Order
		station string
		want    string
	}{
		{"press-index R1 queued: the whole route, nothing marked",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusQueued, SourceNode: "PLN_012", DeliveryNode: "PLN_008", ProcessNode: "PLN_012"},
			"", "PLN_012 → ULN_002 → SMN_BUF_100 → PLN_008"},
		{"press-index R1 parked for RELEASE: the press is the current stop",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusStaged, WaitIndex: 0},
			"PLN_012", "[PLN_012] → ULN_002 → SMN_BUF_100 → PLN_008"},
		{"press-index R1 released, robot at the outbound",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusInTransit, WaitIndex: 1},
			"ULN_002", "PLN_012 → [ULN_002] → SMN_BUF_100 → PLN_008"},
		{"press-index R1 released, robot between stops",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusInTransit, WaitIndex: 1},
			"LM12", "PLN_012 → ULN_002 → SMN_BUF_100 → PLN_008"},
		{"press-index R1 delivered: nothing marked",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusDelivered, WaitIndex: 1},
			"PLN_008", "PLN_012 → ULN_002 → SMN_BUF_100 → PLN_008"},
		{"single_robot: nine steps, the wait dropped, every visit kept",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusStaged, WaitIndex: 0},
			"", "SMN_01 → IN_STG → [ALN_001] → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001"},
		{"single_robot dispatched, robot at the source",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusDispatched, WaitIndex: 0},
			"SMN_01", "[SMN_01] → IN_STG → ALN_001 → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001"},
		{"single_robot dispatched, robot at inbound staging: the wait_index picks the first visit",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusDispatched, WaitIndex: 0},
			"IN_STG", "SMN_01 → [IN_STG] → ALN_001 → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001"},
		{"single_robot released, robot at inbound staging: the wait_index picks the second visit",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusInTransit, WaitIndex: 1},
			"IN_STG", "SMN_01 → IN_STG → ALN_001 → OUT_STG → [IN_STG] → ALN_001 → OUT_STG → ULN_001"},
		// After the release the last six stops are one segment, and the row
		// says no more than that. The line and outbound staging are each
		// visited twice in it: the robot lifting the old bin off the line
		// (stop 3) and setting the new one down (stop 6) is the same input,
		// status in_transit, wait_index 1, station ALN_001, and reads stop 3;
		// at outbound staging, dropping the old bin (stop 4) and lifting it
		// again (stop 7) both read stop 4.
		{"single_robot released, robot at the line: the first visit, whichever it is on",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusInTransit, WaitIndex: 1},
			"ALN_001", "SMN_01 → IN_STG → [ALN_001] → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001"},
		{"single_robot released, robot at outbound staging: the first visit, whichever it is on",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusInTransit, WaitIndex: 1},
			"OUT_STG", "SMN_01 → IN_STG → ALN_001 → [OUT_STG] → IN_STG → ALN_001 → OUT_STG → ULN_001"},
		{"single_robot released, robot between stops: none of the six marked",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusInTransit, WaitIndex: 1},
			"LM7", "SMN_01 → IN_STG → ALN_001 → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001"},
		{"no steps, moving: no stop marked, wherever the robot is",
			Order{SourceNode: "UTN_014", DeliveryNode: "UTN_013", Status: protocol.StatusInTransit},
			"UTN_014", "UTN_014 → UTN_013"},
		{"a step not resolved yet names its group",
			Order{StepsJSON: `[{"action":"pickup","group":"SMN"},{"action":"dropoff","node":"ALN_002"}]`, Status: protocol.StatusQueued},
			"", "SMN → ALN_002"},
		{"no steps: From → Line → To",
			Order{SourceNode: "UTN_014", ProcessNode: "ALN_003", DeliveryNode: "UTN_013"},
			"", "UTN_014 → ALN_003 → UTN_013"},
		{"no steps, line repeats an end",
			Order{SourceNode: "UTN_014", ProcessNode: "UTN_013", DeliveryNode: "UTN_013"},
			"", "UTN_014 → UTN_013"},
		{"no steps, ends not chosen",
			Order{ProcessNode: "ALN_003"},
			"", "— → ALN_003 → —"},
		{"unreadable steps read as no steps",
			Order{StepsJSON: `[{"action":`, SourceNode: "UTN_014", DeliveryNode: "UTN_013"},
			"", "UTN_014 → UTN_013"},
		{"steps that name no node read as no steps",
			Order{StepsJSON: `[{"action":"wait"}]`, SourceNode: "UTN_014", DeliveryNode: "UTN_013"},
			"", "UTN_014 → UTN_013"},
	}
	for _, c := range cases {
		o := c.order
		if got := routeText(RouteStops(&o, c.station)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
