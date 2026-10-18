package domain

import (
	"reflect"
	"testing"

	"shingo/protocol"
)

// flow_preset_remap_test.go — a recipe from another cell, laid onto this one
// (pick-a-part U7).

func remapRouting(rows ...RoutingNode) []RoutingNode {
	for i := range rows {
		rows[i].Enabled = true
	}
	return rows
}

// THE SOURCE CELL: a two-position press with a back slot each, its own
// supermarket and staging lane. THE TARGET: the same shape under other names,
// with two destinations (the lower sequence wins) and one position fewer.
var (
	remapFrom = ShapeFrame{
		Positions: []string{"A_01", "A_02", "A_03"},
		Routing: remapRouting(
			RoutingNode{CoreNodeName: "A_SUP", Role: RoutingRoleSource, Sequence: 1},
			RoutingNode{CoreNodeName: "A_OUT", Role: RoutingRoleDestination, Sequence: 1},
			RoutingNode{CoreNodeName: "A_STG", Role: RoutingRoleStaging, Sequence: 1},
		),
	}
	remapTo = ShapeFrame{
		Positions: []string{"B_01", "B_02"},
		Routing: remapRouting(
			RoutingNode{CoreNodeName: "B_SUP", Role: RoutingRoleSource, Sequence: 1},
			RoutingNode{CoreNodeName: "B_OUT2", Role: RoutingRoleDestination, Sequence: 2},
			RoutingNode{CoreNodeName: "B_OUT1", Role: RoutingRoleDestination, Sequence: 1},
			RoutingNode{CoreNodeName: "B_STG", Role: RoutingRoleStaging, Sequence: 1},
		),
	}
)

func TestRemapShape_PositionsByPictureOrderAndRoutingByRole(t *testing.T) {
	src := []FlowCell{{
		CoreNodeName: "A_01", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PayloadCode: "SHOULD-GO", PairedCoreNode: "A_02", InboundStaging: "A_STG",
		InboundSource: "A_SUP", OutboundDestination: "A_OUT", ChangeoverEvacNodes: []string{"A_01", "A_02"},
	}}
	got, unmapped := RemapShape(src, remapFrom, remapTo)
	want := []FlowCell{{
		CoreNodeName: "B_01", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "B_02", InboundStaging: "B_STG",
		InboundSource: "B_SUP", OutboundDestination: "B_OUT1", ChangeoverEvacNodes: []string{"B_01", "B_02"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("remap:\n got  %+v\n want %+v", got, want)
	}
	if len(unmapped) != 0 {
		t.Errorf("a shape that maps whole listed %v as unmapped", unmapped)
	}
}

// WHAT DOES NOT RESOLVE IS BLANKED AND LISTED, never guessed: a partner past
// the target's last position, a route of vendor-map waypoints drawn for the
// other cell, a destination the source's routing set never had. A position
// with no counterpart is dropped and listed by its source name.
func TestRemapShape_BlanksAndListsWhatDoesNotResolve(t *testing.T) {
	src := []FlowCell{
		{CoreNodeName: "A_02", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
			PairedCoreNode: "A_03", KeyRoute: []string{"LM1", "LM2"}, OutboundDestination: "ELSEWHERE",
			ChangeoverEvacNodes: []string{"A_02", "A_03"}},
		{CoreNodeName: "A_03", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot},
	}
	got, unmapped := RemapShape(src, remapFrom, remapTo)
	want := []FlowCell{{CoreNodeName: "B_02", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("remap:\n got  %+v\n want %+v", got, want)
	}
	if w := []string{"A_03", "B_02"}; !reflect.DeepEqual(unmapped, w) {
		t.Errorf("unmapped = %v, want %v (the dropped source position, then the cell to check)", unmapped, w)
	}
}
