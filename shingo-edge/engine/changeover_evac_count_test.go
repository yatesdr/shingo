package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/comingvectors"
	"shingoedge/release"
	"shingoedge/store/processes"
)

// TestChangeoverEvac_TheCountIsTheSeatsOrTheReleaseIsRefused: with no
// operator count, a changeover evac sends back the seat's count (the landing
// seeds it from Core's, the ticks count it down). When the seat cannot be
// read the count is unknown: the release is refused rather than sent as
// release_empty, which would clear a carrier that may be full.
func TestChangeoverEvac_TheCountIsTheSeatsOrTheReleaseIsRefused(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "EVAC-COUNT", PayloadCode: "EVAC-PART", UOPCapacity: 20, InitialUOP: 13})
	eng := testEngine(t, db)

	got, err := evacDispositionForTask(eng, processes.NodeTask{ProcessNodeID: nodeID, NodeName: "EVAC-COUNT"}, ReleaseDisposition{})
	if err != nil || got.Mode != DispositionSendPartialBack || got.PartialCount == nil || *got.PartialCount != 13 {
		t.Errorf("a counted seat: %+v (%v), want send_partial_back with its 13", got, err)
	}
	got, err = evacDispositionForTask(eng, processes.NodeTask{ProcessNodeID: nodeID + 1000, NodeName: "UNREAD"}, ReleaseDisposition{})
	if err == nil {
		t.Errorf("an unread seat: %+v, want the release refused, not %s", got, got.Mode)
	}
}

// TestChangeoverLift_EachModesLifterIsToldFromItsSteps is the table the
// changeover's release rule reads: per swap mode, the one leg whose release
// lifts the line's cart (release.Facts' departing role at its process node),
// named with the task pointer the planner records it under (assignDispatch: a
// single-order shape's A is the supply pointer and its B the evac pointer; a
// role-declared pair is named by its role). On a release that ships a count
// (the consume arm) that leg carries it (evacDispositionForTask) whichever
// pointer it is under, and every other leg carries none; a produce release
// ships none, and its legs keep their pointer's (release.PlanChangeover). The
// swap legs are the shared vectors' (shared/comingvectors), every role and
// geometry; the tooling and per-position shapes, which the vectors do not
// carry, the builders'.
//
// Sequential's lifter is its one order, under the SUPPLY pointer: the leg that
// took a counted-out cart back to its market with its part (DECISIONS U.1,
// LINE-2). Sequential's tooling leg lifts before its only wait, at dispatch,
// so no release of it lifts anything: "none".
func TestChangeoverLift_EachModesLifterIsToldFromItsSteps(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"single_robot/swap":             "B (evac)",
		"sequential/swap":               "A (supply)",
		"two_robot/swap":                "evac (evac)",
		"two_robot_press_index/swap":    "evac (evac)",
		"press_position/swap":           "A (supply)",
		"single_robot/tooling":          "B (evac)",
		"sequential/tooling":            "none",
		"two_robot/tooling":             "evac (evac)",
		"two_robot_press_index/tooling": "evac (evac)",
	}
	pointer := map[string]string{"A": "supply", "B": "evac", "evac": "evac", "supply": "supply"}
	lifters := map[string][]string{} // a shape (mode, role, geometry, situation) → its lifting legs
	shapes := map[string]string{}    // a shape → its row of the table
	leg := func(shape, row, name string, steps []protocol.ComplexOrderStep) {
		shapes[shape] = row
		if len(steps) > 0 && release.FactsFromSteps(steps, "LINE").Role == release.RoleDeparting {
			lifters[shape] = append(lifters[shape], name+" ("+pointer[name]+")")
		}
	}
	cases, err := comingvectors.Cases()
	testutil.MustNoErr(t, err, "the vectors")
	for _, c := range cases {
		parts := strings.Split(c.Name, "/")
		if len(parts) < 4 || parts[len(parts)-2] != "changeover" {
			continue
		}
		var steps []protocol.ComplexOrderStep
		testutil.MustNoErr(t, json.Unmarshal(c.Steps, &steps), c.Name)
		leg(strings.Join(parts[:len(parts)-1], "/"), parts[0]+"/swap", parts[len(parts)-1], steps)
	}
	for _, mode := range []protocol.SwapMode{protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot,
		protocol.SwapModeTwoRobotPressIndex, protocol.SwapModeSequential, pressPositionSwapMode} {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
			from := &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: mode, Role: role, InboundSource: "IN",
				InboundStaging: "IN-STG", OutboundStaging: "OUT-STG", OutboundDestination: "OUT", PayloadCode: "PART"}
			if mode != protocol.SwapModeSingleRobot && mode != pressPositionSwapMode {
				from.PairedCoreNode = "B"
			}
			to := *from
			to.PayloadCode = "NEXT"
			shape, row, d := fmt.Sprintf("%s/%s/tooling", mode, role), string(mode)+"/tooling",
				BuildEvacuateChangeoverSteps(from, &to, "B", "LINE")
			if mode == pressPositionSwapMode {
				shape, row, d = fmt.Sprintf("%s/%s/swap", mode, role), string(mode)+"/swap",
					BuildSwapChangeoverSteps(from, &to, "B", "LINE")
			}
			if d.Roles != nil {
				leg(shape, row, "evac", d.Roles.evac.steps)
				leg(shape, row, "supply", d.Roles.supply.steps)
				continue
			}
			leg(shape, row, "A", d.StepsA)
			leg(shape, row, "B", d.StepsB)
		}
	}
	names := make([]string, 0, len(shapes))
	for shape := range shapes {
		names = append(names, shape)
	}
	sort.Strings(names)
	rows := map[string]bool{}
	for _, shape := range names {
		row, got := shapes[shape], "none"
		rows[row] = true
		switch len(lifters[shape]) {
		case 0:
		case 1:
			got = lifters[shape][0]
		default:
			t.Errorf("%s: %d legs lift the line's cart (%v): the count cannot be told to one", shape,
				len(lifters[shape]), lifters[shape])
			continue
		}
		if got != want[row] {
			t.Errorf("%s: the line's cart is lifted by %s; the table says %s (row %s)", shape, got, want[row], row)
		}
	}
	for row := range want {
		if !rows[row] {
			t.Errorf("row %s is in the table but no shape reached it", row)
		}
	}
}
