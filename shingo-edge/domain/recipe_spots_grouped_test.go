package domain

import (
	"fmt"
	"testing"

	"shingo/protocol"
)

// recipe_spots_grouped_test.go — what Press 6 cannot show of the grouped rule:
// lanes (a group's list, or an enabled routing lane, D7), a sequential pair
// moved as a pair, and a place no group holds. Press 6's own shape is pinned
// through the router (www/handlers_flow_spots_test.go).

func groupedPositions(names ...string) []Node {
	station := int64(1)
	var out []Node
	for i, n := range names {
		out = append(out, Node{CoreNodeName: n, Sequence: i + 1, OperatorStationID: &station})
	}
	return out
}

func spotsByNode(spots []RecipeSpot) map[string]RecipeSpot {
	out := map[string]RecipeSpot{}
	for _, s := range spots {
		out[s.Node] = s
	}
	return out
}

func wantRefused(t *testing.T, recipe []FlowCell, groups []RecipeGroup, rules SpotRules, moves map[string]string, says string) {
	t.Helper()
	err := RefuseSpotMoves(recipe, groups, nil, rules, moves)
	if says == "" && err != nil {
		t.Errorf("%v refused: %v", moves, err)
	}
	if says != "" && (err == nil || err.Error() != says) {
		t.Errorf("%v: %v, want %q", moves, err, says)
	}
}

// A lane stands on its list at its spot's stand, or on an enabled routing
// lane; a spot's move brings the lane its new stand's list needs.
func TestRecipeSpots_AGroupedLaneStandsOnItsListOrAnEnabledLane(t *testing.T) {
	t.Parallel()
	recipe := []FlowCell{{CoreNodeName: "W1", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
		InboundStaging: "L1", OutboundStaging: "L2", OutboundDestination: "OUT"}}
	groups := []RecipeGroup{{Of: "W1", Name: "Weld", Spots: []GroupSpot{
		{CoreNodeName: "W1", InboundStaging: []string{"L1"}, OutboundStaging: []string{"L2"}},
		{CoreNodeName: "W2", InboundStaging: []string{"L3"}, OutboundStaging: []string{"L2"}},
	}}}
	rules := SpotRules{Positions: groupedPositions("W1", "W2", "W3"), Lanes: []string{"L4"}}
	got := spotsByNode(RecipeSpots(recipe, groups, nil, rules))
	want := map[string]RecipeSpot{
		"W1": {Node: "W1", Job: SpotJobSpot, Group: "Weld", Targets: []string{"W1", "W2"},
			Carry: map[string]map[string]string{"W2": {"L1": "L3"}}},
		"L1": {Node: "L1", Job: SpotJobLane, Of: "W1", Group: "Weld", Targets: []string{"L1", "L4"},
			At: map[string][]string{"W1": {"L1", "L4"}, "W2": {"L3", "L4"}}},
		"L2": {Node: "L2", Job: SpotJobLane, Of: "W1", Group: "Weld", Targets: []string{"L2", "L4"},
			At: map[string][]string{"W1": {"L2", "L4"}, "W2": {"L2", "L4"}}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("places:\n got %+v\nwant %+v", got, want)
	}
	wantRefused(t, recipe, groups, rules, map[string]string{"W1": "W2"},
		"L1 is not a staging lane Weld has for W2 — an engineer adds one on the desktop")
	wantRefused(t, recipe, groups, rules, map[string]string{"L1": "L2"},
		"L2 is not a staging lane Weld has for W1 — an engineer adds one on the desktop")
	wantRefused(t, recipe, groups, rules, map[string]string{"W1": "W3"},
		"W3 is not a spot of Weld — an engineer adds it on the desktop")
	wantRefused(t, recipe, groups, rules, map[string]string{"W1": "W2", "L1": "L3"}, "")
	wantRefused(t, recipe, groups, rules, map[string]string{"L1": "L4"}, "")
	wantRefused(t, recipe, groups, rules, map[string]string{"L1": "L4", "L2": "L4"}, "L4 is already in this recipe")
}

// A sequential pair moves as a pair: the A side is the spot and the B side,
// which owns a row of its own, follows it.
func TestRecipeSpots_AGroupedSequentialPairMovesAsAPair(t *testing.T) {
	t.Parallel()
	seq := func(a, b string) FlowCell {
		return FlowCell{CoreNodeName: a, Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential, PairedCoreNode: b}
	}
	recipe := []FlowCell{seq("A", "B"), seq("B", "A")}
	groups := []RecipeGroup{{Of: "A", Name: "Line", Spots: []GroupSpot{
		{CoreNodeName: "A", PairedCoreNode: []string{"B"}},
		{CoreNodeName: "C", PairedCoreNode: []string{"D"}},
	}}}
	rules := SpotRules{Positions: groupedPositions("A", "B", "C", "D", "E")}
	got := spotsByNode(RecipeSpots(recipe, groups, nil, rules))
	want := map[string]RecipeSpot{
		"A": {Node: "A", Job: SpotJobSpot, Group: "Line", Targets: []string{"A", "C"},
			Carry: map[string]map[string]string{"C": {"B": "D"}}},
		"B": {Node: "B", Job: SpotJobSpot, Of: "A", Group: "Line", Targets: []string{"B"},
			At: map[string][]string{"A": {"B"}, "C": {"D"}}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("places:\n got %+v\nwant %+v", got, want)
	}
	wantRefused(t, recipe, groups, rules, map[string]string{"A": "C"},
		"B is not a partner Line has for C — an engineer adds one on the desktop")
	wantRefused(t, recipe, groups, rules, map[string]string{"B": "D"},
		"D is not a partner Line has for A — an engineer adds one on the desktop")
	wantRefused(t, recipe, groups, rules, map[string]string{"A": "C", "B": "D"}, "")
	if got := LegalSpotMoves(recipe, groups, nil, rules, map[string]string{"A": "C"}); got != nil {
		t.Errorf("half a pair pre-filled: %v", got)
	}
}

// A place no group holds stays where it is, and a stand whose list offers its
// deck only a node another place of the recipe uses is not offered.
func TestRecipeSpots_APlaceNoGroupHoldsStays(t *testing.T) {
	t.Parallel()
	recipe := []FlowCell{
		{CoreNodeName: "P1", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
			PairedCoreNode: "D1", OutboundDestination: "OUT"},
		{CoreNodeName: "LD", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeManualSwap, OutboundDestination: "OUT"},
	}
	groups := []RecipeGroup{{Of: "P1", Name: "Left Hand", Spots: []GroupSpot{
		{CoreNodeName: "P1", PairedCoreNode: []string{"D1"}},
		{CoreNodeName: "P2", PairedCoreNode: []string{"LD"}},
		{CoreNodeName: "P3", PairedCoreNode: []string{"D1"}},
	}}}
	rules := SpotRules{Positions: groupedPositions("P1", "P2", "P3", "D1", "LD", "X")}
	got := spotsByNode(RecipeSpots(recipe, groups, nil, rules))
	if p := got["P1"]; fmt.Sprint(p.Targets) != "[P1 P3]" || p.Carry != nil {
		t.Errorf("P1 = %+v, want P1 and P3 (P2's only deck is the loader card's node)", p)
	}
	if p := got["LD"]; fmt.Sprint(p.Targets) != "[LD]" || p.Group != "" {
		t.Errorf("LD = %+v, want a place that stays", p)
	}
	wantRefused(t, recipe, groups, rules, map[string]string{"LD": "X"}, "LD is in no group of this recipe; it stays where it is")
	wantRefused(t, recipe, groups, rules, map[string]string{"P1": "P2", "D1": "LD"}, "LD is already in this recipe")
	wantRefused(t, recipe, groups, rules, map[string]string{"P1": "P3"}, "")
}
