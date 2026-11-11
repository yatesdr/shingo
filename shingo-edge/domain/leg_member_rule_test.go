package domain

import (
	"strings"
	"testing"

	"shingo/protocol"
)

// leg_member_rule_test.go — where Core picks, a line names a node group or a
// node in no group, never a member of one (owner, 2026-11-11). A source and a
// destination are where Core picks; a staging spot names the exact node.

// memberKinds is Core's list as the engine holds it: a group with one member,
// a node in no group, a staging spot, and the group map. A slot inside a lane
// is never in the list.
func memberKinds() CoreNodeKinds {
	return CoreNodeKindsOf(map[string]protocol.NodeInfo{
		"MR-GRP":  {Name: "MR-GRP", NodeType: protocol.NodeClassNGRP},
		"MR-SLOT": {Name: "MR-SLOT"},
		"MR-NODE": {Name: "MR-NODE"},
		"MR-STG":  {Name: "MR-STG"},
	}, map[string][]string{"MR-GRP": {"MR-SLOT"}})
}

// TestLegRefusal_WhereCorePicksAMemberIsRefused: every claim leg, handed a
// member, a lane slot (a name the list does not hold), the group and a node in
// no group. Where Core picks the member and the slot are refused, the member's
// refusal naming its group; a staging leg takes both.
func TestLegRefusal_WhereCorePicksAMemberIsRefused(t *testing.T) {
	t.Parallel()
	kinds := memberKinds()
	picks := map[string]bool{"inbound_source": true, "outbound_destination": true,
		"containment_destination": true, "partial_destination": true}
	for _, leg := range ClaimLegs(NodeClaimInput{}) {
		for name, refusedWherePicked := range map[string]bool{
			"MR-SLOT": true, "MR-GRP.MR-SLOT": true, "MR-LANE-S1": true, "MR-GRP": false, "MR-NODE": false,
		} {
			got := kinds.LegRefusal(leg.Role, name)
			want := refusedWherePicked && picks[leg.Field]
			if (got != "") != want {
				t.Errorf("%s naming %q: refusal %q, want refused=%v", leg.Field, name, got, want)
			}
			if want && strings.HasPrefix(name, "MR-") && name != "MR-LANE-S1" && !strings.Contains(got, `"MR-GRP"`) {
				t.Errorf("%s naming the member %q: %q does not name the group to use", leg.Field, name, got)
			}
		}
	}
}

// TestRefusedLegs_AClaimNamingAMemberIsRefusedOnItsField: the editor's error is
// on the field that names the member, and only there.
func TestRefusedLegs_AClaimNamingAMemberIsRefusedOnItsField(t *testing.T) {
	t.Parallel()
	in := NodeClaimInput{CoreNodeName: "MR-NODE", Role: protocol.ClaimRoleProduce,
		InboundSource: "MR-GRP", OutboundDestination: "MR-SLOT", ContainmentDestination: "MR-LANE-S1",
		InboundStaging: "MR-SLOT", OutboundStaging: "MR-STG"}
	got := map[string]bool{}
	for _, f := range memberKinds().RefusedLegs(in) {
		got[f.Field] = true
	}
	if !got["outbound_destination"] || !got["containment_destination"] || len(got) != 2 {
		t.Errorf("refused fields %v, want outbound_destination and containment_destination only", got)
	}
}

// TestRefusedPresetLegs_ARecipeNamingAMemberIsRefused: a recipe's source and
// destination follow the claim's rule; its staging names the exact node.
func TestRefusedPresetLegs_ARecipeNamingAMemberIsRefused(t *testing.T) {
	t.Parallel()
	cells := []FlowCell{
		{CoreNodeName: "MR-NODE", InboundSource: "MR-GRP", OutboundDestination: "MR-SLOT", InboundStaging: "MR-SLOT"},
		{CoreNodeName: "MR-NODE", InboundSource: "MR-LANE-S1", OutboundDestination: "MR-NODE"},
	}
	var got []string
	for _, r := range RefusedPresetLegs(cells, memberKinds()) {
		got = append(got, r.Field+"@"+string(rune('0'+r.Cell)))
	}
	if strings.Join(got, " ") != "outbound_destination@1 inbound_source@2" {
		t.Errorf("refused %v, want cell 1's outbound destination and cell 2's inbound source", got)
	}
}

// TestLegRefusal_NoNodeListRefusesNothing: before Core's list is heard a
// member is not known to be one, and no name is unknown.
func TestLegRefusal_NoNodeListRefusesNothing(t *testing.T) {
	t.Parallel()
	empty := CoreNodeKindsOf(nil, nil)
	for _, name := range []string{"MR-SLOT", "MR-GRP.MR-SLOT", "MR-LANE-S1"} {
		if got := empty.LegRefusal(RoutingRoleDestination, name); got != "" {
			t.Errorf("no list heard, %q refused: %q", name, got)
		}
	}
}
