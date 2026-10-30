package domain

import (
	"encoding/json"
	"reflect"
	"testing"

	"shingo/protocol"
)

func routedSet(rows ...RoutingNode) []RoutingNode { return rows }

func routed(name, role string, seq int) RoutingNode {
	return RoutingNode{CoreNodeName: name, Role: role, Sequence: seq, Enabled: true}
}

func positionsOf(names ...string) []Node {
	out := make([]Node, 0, len(names))
	for _, n := range names {
		out = append(out, Node{CoreNodeName: n})
	}
	return out
}

// TestReadRecipe_ByRole is the rule, one field at a time.
func TestReadRecipe_ByRole(t *testing.T) {
	t.Parallel()
	dest := func(v string) FlowCell {
		return FlowCell{CoreNodeName: "PLN_01", OutboundDestination: v}
	}
	off := routed("DST_OFF", RoutingRoleDestination, 5)
	off.Enabled = false
	const none = "R has no outbound destination — add one to this process's routing set"
	// A stored name stays a name when the role has no node: this is the "rest" the
	// migration flags, and the re-save sentence is still true of it.
	const gone = "R sends cell 1 to DST_GONE, which is no longer a position here — an engineer re-saves the recipe on the desktop"
	for _, tc := range []struct {
		name     string
		stored   string
		routing  []RoutingNode
		want     string
		findings []string
	}{
		{"one node, the role", RecipeFollowsDestination, routedSet(routed("DST_A", RoutingRoleDestination, 0)), "DST_A", nil},
		{"one node, an old name follows it", "DST_GONE", routedSet(routed("DST_A", RoutingRoleDestination, 0)), "DST_A", nil},
		{"several, the chosen name", "DST_B", routedSet(routed("DST_A", RoutingRoleDestination, 0), routed("DST_B", RoutingRoleDestination, 1)), "DST_B", nil},
		{"several, a name gone orphans", "DST_GONE", routedSet(routed("DST_A", RoutingRoleDestination, 0), routed("DST_B", RoutingRoleDestination, 1)), "DST_GONE", []string{gone}},
		{"several, the role takes the default", RecipeFollowsDestination, routedSet(routed("DST_B", RoutingRoleDestination, 1), routed("DST_A", RoutingRoleDestination, 0)), "DST_A", nil},
		{"none, the role", RecipeFollowsDestination, nil, "", []string{none}},
		{"none, a name gone orphans", "DST_GONE", nil, "DST_GONE", []string{gone}},
		{"none, a listed name switched off stays", "DST_OFF", routedSet(off), "DST_OFF", nil},
		{"a position stays whatever the set holds", "PLN_02", routedSet(routed("DST_A", RoutingRoleDestination, 0)), "PLN_02", nil},
		{"a blank stays blank", "", routedSet(routed("DST_A", RoutingRoleDestination, 0)), "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := ReadRecipe("R", []FlowCell{dest(tc.stored)}, nil, positionsOf("PLN_01", "PLN_02"), tc.routing)
			if got := read.Cells[0].OutboundDestination; got != tc.want {
				t.Errorf("destination = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(read.Findings, tc.findings) {
				t.Errorf("findings = %q, want %q", read.Findings, tc.findings)
			}
		})
	}
}

// TestReadRecipe_LeavesTheCallersRowsAlone: the card's rows are the caller's, and
// the recipe a second read sees is the stored one.
func TestReadRecipe_LeavesTheCallersRowsAlone(t *testing.T) {
	t.Parallel()
	cells := []FlowCell{{CoreNodeName: "PLN_01", InboundSource: RecipeFollowsSource}}
	spots := []GroupSpot{{CoreNodeName: "PLN_01", InboundStaging: []string{"STG_OLD"}}}
	groups := []RecipeGroup{{Of: "PLN_01", Name: "G", Spots: spots}}
	read := ReadRecipe("R", cells, groups, positionsOf("PLN_01"), routedSet(routed("SRC_A", RoutingRoleSource, 0), routed("STG_A", RoutingRoleStaging, 0)))
	if read.Cells[0].InboundSource != "SRC_A" || read.Groups[0].Spots[0].InboundStaging[0] != "STG_A" {
		t.Fatalf("read = %+v, want the set's nodes", read)
	}
	if cells[0].InboundSource != RecipeFollowsSource || spots[0].InboundStaging[0] != "STG_OLD" {
		t.Errorf("the stored rows moved: %+v, %+v", cells, spots)
	}
}

// TestReadRecipe_AStagingListFollowsItsLane: a group's lane list is the lanes the
// recipe may stand on, so one lane in the set is the list and a position stays;
// with several the list is the recipe's and can orphan.
func TestReadRecipe_AStagingListFollowsItsLane(t *testing.T) {
	t.Parallel()
	groups := []RecipeGroup{{Of: "PLN_01", Name: "G", Spots: []GroupSpot{
		{CoreNodeName: "PLN_01", InboundStaging: []string{"STG_OLD", "PLN_02"}}}}}
	cells := []FlowCell{{CoreNodeName: "PLN_01"}}
	one := ReadRecipe("R", cells, groups, positionsOf("PLN_01", "PLN_02"), routedSet(routed("STG_A", RoutingRoleStaging, 0)))
	if got, want := one.Groups[0].Spots[0].InboundStaging, []string{"STG_A", "PLN_02"}; !reflect.DeepEqual(got, want) {
		t.Errorf("one lane: %v, want %v", got, want)
	}
	if len(one.Findings) != 0 {
		t.Errorf("one lane: findings %q", one.Findings)
	}
	several := ReadRecipe("R", cells, groups, positionsOf("PLN_01", "PLN_02"),
		routedSet(routed("STG_A", RoutingRoleStaging, 0), routed("STG_B", RoutingRoleStaging, 1)))
	want := "R names STG_OLD in G, which is no longer a position here — an engineer re-saves the recipe on the desktop"
	if len(several.Findings) != 1 || several.Findings[0] != want {
		t.Errorf("several lanes: findings %q, want %q", several.Findings, want)
	}
}

// TestCollapseRecipeRouting_StoresOneNodeRolesByRole: the save's half. One-node
// roles store the role; a position, a blank and a role with several keep what they
// have; the partial home is the recipe's own; the groups are stored as they came;
// and a second pass changes nothing.
func TestCollapseRecipeRouting_StoresOneNodeRolesByRole(t *testing.T) {
	t.Parallel()
	flow, err := RecipeShapeJSON([]FlowCell{{
		CoreNodeName: "PLN_01", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
		InboundSource: "SRC_A", OutboundDestination: "DST_B", InboundStaging: "PLN_02", OutboundStaging: "",
		ChangeoverEvacDestination: "DST_A", PartialDestination: "DOCK",
	}, {
		CoreNodeName: "PLN_03", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
		InboundSource: "SRC_A",
	}}, json.RawMessage(`[{"of":"PLN_01","name":"G","spots":[{"core_node_name":"PLN_01"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	routing := routedSet(routed("SRC_A", RoutingRoleSource, 0), routed("DST_A", RoutingRoleDestination, 0), routed("DST_B", RoutingRoleDestination, 1))
	out, err := CollapseRecipeRouting(flow, positionsOf("PLN_01", "PLN_02", "PLN_03"), routing)
	if err != nil {
		t.Fatal(err)
	}
	cells, groups, err := ParseRecipe(out)
	if err != nil {
		t.Fatal(err)
	}
	c := cells[0]
	if c.InboundSource != RecipeFollowsSource || cells[1].InboundSource != RecipeFollowsSource {
		t.Errorf("source = %q and %q, want the role", c.InboundSource, cells[1].InboundSource)
	}
	if c.OutboundDestination != "DST_B" || c.ChangeoverEvacDestination != "DST_A" {
		t.Errorf("destinations = %q, %q: a role with several keeps its names", c.OutboundDestination, c.ChangeoverEvacDestination)
	}
	if c.InboundStaging != "PLN_02" || c.OutboundStaging != "" || c.PartialDestination != "DOCK" {
		t.Errorf("staging %q/%q partial %q: a position, a blank and a partial home stay", c.InboundStaging, c.OutboundStaging, c.PartialDestination)
	}
	if len(groups) != 1 || groups[0].Name != "G" {
		t.Errorf("groups = %+v, want them as they came", groups)
	}
	again, err := CollapseRecipeRouting(out, positionsOf("PLN_01", "PLN_02", "PLN_03"), routing)
	if err != nil || again != out {
		t.Errorf("a second pass moved it: %v\n%s\n%s", err, out, again)
	}
	if key, stored := RecipeStructureKey(cells), RecipeStructureKey([]FlowCell{{
		CoreNodeName: "PLN_01", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot,
		InboundSource: "SRC_A", OutboundDestination: "DST_B", InboundStaging: "PLN_02",
		ChangeoverEvacDestination: "DST_A", PartialDestination: "DOCK",
	}, {
		CoreNodeName: "PLN_03", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSingleRobot, InboundSource: "SRC_A",
	}}); key != stored {
		t.Errorf("the role-only form is another structure: a save under the same name would be refused\n%q\n%q", key, stored)
	}
}
