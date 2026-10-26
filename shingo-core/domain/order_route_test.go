package domain

import (
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
)

// routeText draws stops the way a person reads them: "→" between stops, an
// unassigned end as "—", and each stop's state after a colon, a remaining stop
// bare.
func routeText(stops []RouteStop) string {
	parts := make([]string, len(stops))
	for i, s := range stops {
		p := s.Node
		if s.Unassigned {
			p = "—"
		}
		if s.State != RouteRemaining {
			p += ":" + string(s.State)
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

// A plan with a bare wait between a drop and a lift at the same node, and one
// whose next lift after the bare wait is somewhere else.
const (
	routeBareSameSteps = `[{"action":"pickup","node":"SMN_01"},{"action":"dropoff","node":"ALN_001"},{"action":"wait"},` +
		`{"action":"pickup","node":"ALN_001"},{"action":"dropoff","node":"ULN_001"}]`
	routeBareAwaySteps = `[{"action":"pickup","node":"SMN_01"},{"action":"dropoff","node":"IN_STG"},{"action":"wait"},` +
		`{"action":"pickup","node":"ALN_001"},{"action":"dropoff","node":"ULN_001"}]`
	// A lane wait parks the robot at a map point, which is no stop.
	routeLaneWaitSteps = `[{"action":"pickup","node":"SMN_01"},{"action":"wait","node":"AP7","wait_kind":"lane"},` +
		`{"action":"dropoff","node":"SLN_005"}]`
)

func TestRouteStops(t *testing.T) {
	t.Parallel()
	single := func(status protocol.Status, waitIndex, done int) Order {
		return Order{StepsJSON: routeSingleSteps, Status: status, WaitIndex: waitIndex, StepsDone: done}
	}
	cases := []struct {
		name  string
		order Order
		want  string
	}{
		{"press-index R1 queued: the whole route, the press next",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusQueued, SourceNode: "PLN_012", DeliveryNode: "PLN_008", ProcessNode: "PLN_012"},
			"PLN_012:next → ULN_002 → SMN_BUF_100 → PLN_008:final"},
		{"press-index R1 parked for RELEASE: the robot is at the press",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusStaged, WaitIndex: 0, StepsDone: 1},
			"PLN_012:here → ULN_002:next → SMN_BUF_100 → PLN_008:final"},
		{"press-index R1 staged before its wait block is counted: the wait_index puts it at the press",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusStaged, WaitIndex: 0},
			"PLN_012:here → ULN_002:next → SMN_BUF_100 → PLN_008:final"},
		{"press-index R1 released, the press lifted: it has left the press for the outbound",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusInTransit, WaitIndex: 1, StepsDone: 2},
			"PLN_012:left → ULN_002:next → SMN_BUF_100 → PLN_008:final"},
		{"press-index R1 released, dropped at the outbound and on its way",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusInTransit, WaitIndex: 1, StepsDone: 3},
			"PLN_012:passed → ULN_002:left → SMN_BUF_100:next → PLN_008:final"},
		{"press-index R1 delivered: every stop behind it, the last one where it is",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusDelivered, WaitIndex: 1},
			"PLN_012:passed → ULN_002:passed → SMN_BUF_100:passed → PLN_008:here"},
		{"single_robot: nine steps, the wait dropped, every visit kept",
			single(protocol.StatusStaged, 0, 3),
			"SMN_01:passed → IN_STG:passed → ALN_001:here → OUT_STG:next → IN_STG → ALN_001 → OUT_STG → ULN_001:final"},
		{"single_robot dispatched, nothing finished yet: no stop here or left, the source next",
			single(protocol.StatusDispatched, 0, 0),
			"SMN_01:next → IN_STG → ALN_001 → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001:final"},
		{"single_robot dispatched, the source lifted: it has left the source",
			single(protocol.StatusDispatched, 0, 1),
			"SMN_01:left → IN_STG:next → ALN_001 → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001:final"},
		{"single_robot dispatched, dropped at inbound staging, the line next",
			single(protocol.StatusInTransit, 0, 2),
			"SMN_01:passed → IN_STG:left → ALN_001:next → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001:final"},
		{"single_robot released, nothing of the tail done: standing at the line, its next work there",
			single(protocol.StatusInTransit, 1, 3),
			"SMN_01:passed → IN_STG:passed → ALN_001:here → OUT_STG:next → IN_STG → ALN_001 → OUT_STG → ULN_001:final"},
		// The line and outbound staging are each visited twice after the
		// release; steps_done tells the visits apart.
		{"single_robot released, the old bin lifted off the line: the first visit",
			single(protocol.StatusInTransit, 1, 4),
			"SMN_01:passed → IN_STG:passed → ALN_001:left → OUT_STG:next → IN_STG → ALN_001 → OUT_STG → ULN_001:final"},
		{"single_robot released, the old bin parked at outbound staging: the first visit",
			single(protocol.StatusInTransit, 1, 5),
			"SMN_01:passed → IN_STG:passed → ALN_001:passed → OUT_STG:left → IN_STG:next → ALN_001 → OUT_STG → ULN_001:final"},
		{"single_robot released, the new bin collected at inbound staging: the second visit",
			single(protocol.StatusInTransit, 1, 6),
			"SMN_01:passed → IN_STG:passed → ALN_001:passed → OUT_STG:passed → IN_STG:left → ALN_001:next → OUT_STG → ULN_001:final"},
		{"single_robot released, the new bin set down on the line: the second visit",
			single(protocol.StatusInTransit, 1, 7),
			"SMN_01:passed → IN_STG:passed → ALN_001:passed → OUT_STG:passed → IN_STG:passed → ALN_001:left → OUT_STG:next → ULN_001:final"},
		{"single_robot released, the old bin lifted again at outbound staging: the market next",
			single(protocol.StatusInTransit, 1, 8),
			"SMN_01:passed → IN_STG:passed → ALN_001:passed → OUT_STG:passed → IN_STG:passed → ALN_001:passed → OUT_STG:left → ULN_001:next"},
		{"single_robot in transit with every step done, not yet delivered: here at the market",
			single(protocol.StatusInTransit, 1, 9),
			"SMN_01:passed → IN_STG:passed → ALN_001:passed → OUT_STG:passed → IN_STG:passed → ALN_001:passed → OUT_STG:passed → ULN_001:here"},
		{"single_robot cancelled after the old bin left the line: no robot here or next",
			single(protocol.StatusCancelled, 1, 4),
			"SMN_01:passed → IN_STG:passed → ALN_001:passed → OUT_STG → IN_STG → ALN_001 → OUT_STG → ULN_001:final"},
		{"a bare wait, parked: the robot is where it last dropped",
			Order{StepsJSON: routeBareSameSteps, Status: protocol.StatusStaged, StepsDone: 2},
			"SMN_01:passed → ALN_001:here → ULN_001:next"},
		{"a bare wait, released to lift where it stands: still here",
			Order{StepsJSON: routeBareSameSteps, Status: protocol.StatusInTransit, WaitIndex: 1, StepsDone: 2},
			"SMN_01:passed → ALN_001:here → ULN_001:next"},
		{"a bare wait, parked away from its next lift",
			Order{StepsJSON: routeBareAwaySteps, Status: protocol.StatusStaged, StepsDone: 2},
			"SMN_01:passed → IN_STG:here → ALN_001:next → ULN_001:final"},
		{"a bare wait, released away from its next lift: the wait_index sends it on, it has left",
			Order{StepsJSON: routeBareAwaySteps, Status: protocol.StatusInTransit, WaitIndex: 1, StepsDone: 2},
			"SMN_01:passed → IN_STG:left → ALN_001:next → ULN_001:final"},
		{"a bare wait crossed and the next lift done",
			Order{StepsJSON: routeBareAwaySteps, Status: protocol.StatusInTransit, WaitIndex: 1, StepsDone: 4},
			"SMN_01:passed → IN_STG:passed → ALN_001:left → ULN_001:next"},
		{"a lane wait at a map point: lifted and parked short of the lane, between its stops",
			Order{StepsJSON: routeLaneWaitSteps, Status: protocol.StatusStaged, StepsDone: 2},
			"SMN_01:left → SLN_005:next"},
		{"a step not resolved yet names its group",
			Order{StepsJSON: `[{"action":"pickup","group":"SMN"},{"action":"dropoff","node":"ALN_002"}]`, Status: protocol.StatusQueued},
			"SMN:next → ALN_002:final"},
		{"no steps, moving with the bin lifted: its two implied steps",
			Order{SourceNode: "UTN_014", DeliveryNode: "UTN_013", Status: protocol.StatusInTransit, StepsDone: 1},
			"UTN_014:left → UTN_013:next"},
		{"no steps: From → Line → To",
			Order{SourceNode: "UTN_014", ProcessNode: "ALN_003", DeliveryNode: "UTN_013"},
			"UTN_014:next → ALN_003 → UTN_013:final"},
		{"no steps, the line between the lift and the drop it does not visit",
			Order{SourceNode: "UTN_014", ProcessNode: "ALN_003", DeliveryNode: "UTN_013", Status: protocol.StatusInTransit, StepsDone: 1},
			"UTN_014:left → ALN_003 → UTN_013:next"},
		{"no steps, line repeats an end",
			Order{SourceNode: "UTN_014", ProcessNode: "UTN_013", DeliveryNode: "UTN_013"},
			"UTN_014:next → UTN_013:final"},
		{"no steps, ends not chosen",
			Order{ProcessNode: "ALN_003"},
			"—:next → ALN_003 → —:final"},
		{"unreadable steps read as no steps",
			Order{StepsJSON: `[{"action":`, SourceNode: "UTN_014", DeliveryNode: "UTN_013"},
			"UTN_014:next → UTN_013:final"},
		{"steps that name no node read as no steps",
			Order{StepsJSON: `[{"action":"wait"}]`, SourceNode: "UTN_014", DeliveryNode: "UTN_013"},
			"UTN_014:next → UTN_013:final"},
	}
	for _, c := range cases {
		o := c.order
		if got := routeText(RouteStops(&o)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// TestStepsDoneAfter: a finished block completes the plan's next open step when
// it is that step's action at that step's node, and nothing else. Each sequence
// is the vendor's blocks in the order it finishes them.
func TestStepsDoneAfter(t *testing.T) {
	t.Parallel()
	type block struct{ at, action string }
	cases := []struct {
		name   string
		order  Order
		blocks []block
		want   []int // steps_done after each block
	}{
		{"single_robot: every block its step, each repeated node by its place",
			Order{StepsJSON: routeSingleSteps},
			[]block{{"SMN_01", "pickup"}, {"IN_STG", "dropoff"}, {"ALN_001", "wait"}, {"ALN_001", "pickup"},
				{"OUT_STG", "dropoff"}, {"IN_STG", "pickup"}, {"ALN_001", "dropoff"}, {"OUT_STG", "pickup"}, {"ULN_001", "dropoff"}},
			[]int{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{"an advanced load sequence: once, on the block that lifts; the turns and the approach count nothing",
			Order{SourceNode: "SMN_01", DeliveryNode: "ALN_001"},
			[]block{{"SMN_01", ""}, {"SMN_01", ""}, {"SMN_01", "pickup"}, {"SMN_01", ""}, {"ALN_001", "dropoff"}},
			[]int{0, 0, 1, 1, 2}},
		{"a bare wait sends no block: the lift after it counts past it",
			Order{StepsJSON: routeBareAwaySteps},
			[]block{{"SMN_01", "pickup"}, {"IN_STG", "dropoff"}, {"ALN_001", "pickup"}, {"ULN_001", "dropoff"}},
			[]int{1, 2, 4, 5}},
		{"a block repeated, or out of its place, counts nothing",
			Order{StepsJSON: routeSingleSteps},
			[]block{{"SMN_01", "pickup"}, {"SMN_01", "pickup"}, {"OUT_STG", "dropoff"}, {"IN_STG", "dropoff"}},
			[]int{1, 1, 1, 2}},
		{"a step not resolved yet takes its action wherever the fleet did it",
			Order{StepsJSON: `[{"action":"pickup","group":"SMN"},{"action":"dropoff","node":"ALN_002"}]`},
			[]block{{"SMN_013", "pickup"}, {"ALN_002", "dropoff"}},
			[]int{1, 2}},
		{"a block past the last step counts nothing",
			Order{SourceNode: "SMN_01", DeliveryNode: "ALN_001", StepsDone: 2},
			[]block{{"ALN_001", "dropoff"}},
			[]int{2}},
	}
	for _, c := range cases {
		o := c.order
		for i, b := range c.blocks {
			if n, ok := StepsDoneAfter(&o, b.at, b.action); ok {
				o.StepsDone = n
			}
			if o.StepsDone != c.want[i] {
				t.Errorf("%s: after block %d (%s at %s) steps_done is %d, want %d", c.name, i+1, b.action, b.at, o.StepsDone, c.want[i])
				break
			}
		}
	}
}

// stripText is the strip as the boards draw it: its beats left to right, a
// stop as state:node, the stops between the next and the final as
// remaining:+n, and the robot's arrow as ➜ before the stop it heads to.
func stripText(v RouteView) string {
	cell := func(s RouteStop) string {
		n := s.Node
		if s.Unassigned {
			n = "—"
		}
		return string(s.State) + ":" + n
	}
	var parts []string
	beat := func(b RouteBeat, s string) {
		if v.Heading == b {
			parts = append(parts, "➜")
		}
		parts = append(parts, s)
	}
	beat(BeatOrigin, cell(v.Origin))
	if v.Here != nil {
		parts = append(parts, cell(*v.Here))
	}
	if v.Next != nil {
		beat(BeatNext, cell(*v.Next))
	}
	if v.Remaining > 0 {
		parts = append(parts, "remaining:+"+strconv.Itoa(v.Remaining))
	}
	if v.Final != nil {
		beat(BeatFinal, cell(*v.Final))
	}
	return strings.Join(parts, " ")
}

// TestRouteViewOf is the strip's four beats (owner, 2026-10-26): the origin,
// the stop the robot is at, the next, the final, and one +n for the stops
// between the next and the final. Passed stops are never drawn, the origin
// stands for them; in transit the stop just left is not drawn either, and the
// arrow before the stop the robot heads to says it is between.
func TestRouteViewOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		order Order
		want  string
	}{
		{"two stops, queued: the origin next, no robot on its way",
			Order{SourceNode: "UTN_014", DeliveryNode: "UTN_013", Status: protocol.StatusQueued},
			"next:UTN_014 final:UTN_013"},
		{"two stops, dispatched before the first block: the arrow before the origin",
			Order{SourceNode: "UTN_014", DeliveryNode: "UTN_013", Status: protocol.StatusDispatched},
			"➜ next:UTN_014 final:UTN_013"},
		{"two stops, lifted and between: the arrow before the final, which is next",
			Order{SourceNode: "UTN_014", DeliveryNode: "UTN_013", Status: protocol.StatusInTransit, StepsDone: 1},
			"left:UTN_014 ➜ next:UTN_013"},
		{"four stops, in transit at a stop, the origin: here is the origin",
			Order{StepsJSON: routeR1Steps, Status: protocol.StatusInTransit, WaitIndex: 1},
			"here:PLN_012 next:ULN_002 remaining:+1 final:PLN_008"},
		{"eight stops, acknowledged before the first block",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusAcknowledged},
			"➜ next:SMN_01 remaining:+6 final:ULN_001"},
		{"eight stops, in transit between: the stop left is not drawn",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusInTransit, StepsDone: 2},
			"passed:SMN_01 ➜ next:ALN_001 remaining:+4 final:ULN_001"},
		{"eight stops, staged at the line",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusStaged, StepsDone: 3},
			"passed:SMN_01 here:ALN_001 next:OUT_STG remaining:+3 final:ULN_001"},
		{"eight stops, delivered: here is the final",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusDelivered},
			"passed:SMN_01 here:ULN_001"},
		{"eight stops cancelled midway: the stops to come, nothing here or next",
			Order{StepsJSON: routeSingleSteps, Status: protocol.StatusCancelled, WaitIndex: 1, StepsDone: 5},
			"passed:SMN_01 remaining:+3 final:ULN_001"},
		{"one stop: the origin alone",
			Order{StepsJSON: `[{"action":"pickup","node":"SLN_005"},{"action":"wait","node":"AP7"}]`, Status: protocol.StatusQueued},
			"next:SLN_005"},
	}
	for _, c := range cases {
		o := c.order
		if got := stripText(RouteViewOf(RouteStops(&o), o.Status)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
