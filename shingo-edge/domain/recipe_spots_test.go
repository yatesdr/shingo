package domain

import (
	"reflect"
	"testing"

	"shingo/protocol"
)

// recipe_spots_test.go — what Press 6 cannot show: a third position, and two
// moves each legal alone that together pair a press with a third no recipe
// set up. Press 6's own shape is pinned through the router and the HMI
// (www/handlers_flow_spots_test.go, composer-modal.test.js).

func thirdCell(head, deck, third string) FlowCell {
	return FlowCell{CoreNodeName: head, Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: deck, SecondPairedCoreNode: third}
}

func TestRecipeSpots_AThirdPositionKeepsItsPairingLikeTheDeck(t *testing.T) {
	t.Parallel()
	station := int64(1)
	var positions []Node
	for i, n := range []string{"P1", "P2", "P3", "D1", "T1", "T2"} {
		positions = append(positions, Node{CoreNodeName: n, Sequence: i + 1, OperatorStationID: &station})
	}
	recipe := []FlowCell{thirdCell("P1", "D1", "T1")}
	rules := SpotRules{Positions: positions, Enabled: [][]FlowCell{
		recipe,
		{thirdCell("P2", "D1", "T1")}, // P2 runs with the same deck and third
		{thirdCell("P1", "D1", "T2")}, // P1 may take T2 as its third
	}}
	got := map[string]RecipeSpot{}
	for _, s := range RecipeSpots(recipe, nil, rules) {
		got[s.Node] = s
	}
	want := map[string]RecipeSpot{
		"P1": {Node: "P1", Job: SpotJobSpot, Targets: []string{"P1", "P2"}},
		"D1": {Node: "D1", Job: SpotJobOnDeck, Of: "P1", Targets: []string{"D1"}},
		"T1": {Node: "T1", Job: SpotJobThird, Of: "P1", Targets: []string{"T1", "T2"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spots = %+v, want %+v", got, want)
	}

	for _, tc := range []struct {
		moves map[string]string
		says  string
	}{
		{map[string]string{"T1": "P3"}, "P3 is not a second paired core node your recipes set up for P1 — an engineer adds one on the desktop"},
		{map[string]string{"P1": "P3"}, "P3 is not a press spot your recipes set up with D1 on deck — an engineer adds one on the desktop"},
		// Each alone is a target; together they pair P2 with T2, which no
		// recipe does (D2 of DECISIONS-spots-v0).
		{map[string]string{"P1": "P2", "T1": "T2"}, "P2 and T2 are not a pairing your recipes set up — an engineer adds one on the desktop"},
	} {
		err := RefuseSpotMoves(recipe, nil, rules, tc.moves)
		if err == nil || err.Error() != tc.says {
			t.Errorf("%v: %v, want %q", tc.moves, err, tc.says)
		}
	}
	for _, ok := range []map[string]string{{"P1": "P2"}, {"T1": "T2"}} {
		if err := RefuseSpotMoves(recipe, nil, rules, ok); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	// The pre-fill obeys the same rule: the pair the server would refuse is
	// not suggested.
	if got := LegalSpotMoves(recipe, nil, rules, map[string]string{"P1": "P2", "T1": "T2"}); got != nil {
		t.Errorf("pre-fill = %v, want none", got)
	}
}
