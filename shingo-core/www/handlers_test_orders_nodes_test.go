package www

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/store/nodes"
)

// TestTestOrdersNodePicker_OffersOnlyValidNodes pins what the /test-orders node
// dropdowns are allowed to show.
//
// They used to be the node table, rendered in table order with nothing but the
// name. A disabled node looked exactly like a live one, and a node group looked
// exactly like a slot — which is the dead end operators hit on the orders page
// before it started badging containers. Same page, same fifteen dropdowns, no
// such treatment.
//
// The rule is not "hide the groups": a group is a legitimate source for a
// group-retrieve. It is "say what each one is, and do not offer what is turned
// off."
func TestTestOrdersNodePicker_OffersOnlyValidNodes(t *testing.T) {
	t.Parallel()

	groupID := int64(1)
	laneID := int64(2)
	all := []*nodes.Node{
		{ID: groupID, Name: "SUPERMARKET", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP},
		{ID: laneID, Name: "SUPERMARKET-L1", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassLANE, ParentID: &groupID},
		{ID: 3, Name: "SUPERMARKET-L1-S1", Enabled: true, Zone: "A", ParentID: &laneID},
		{ID: 4, Name: "LINE1-IN", Enabled: true, Zone: "B"},
		{ID: 5, Name: "DECOMMISSIONED-SLOT", Enabled: false, Zone: "B"},
	}

	opts := buildNodePickerOptions(all)

	byName := map[string]nodePickerOption{}
	for _, o := range opts {
		byName[o.Name] = o
	}

	if _, found := byName["DECOMMISSIONED-SLOT"]; found {
		t.Error("a disabled node is offered as a target; picking it sends a robot to a place that is turned off")
	}
	if len(opts) != 4 {
		t.Errorf("picker has %d entries, want 4 (every enabled node, once): %+v", len(opts), opts)
	}

	group, ok := byName["SUPERMARKET"]
	if !ok {
		t.Fatal("the node group is missing entirely — a group is a valid source for a group-retrieve, so it must stay selectable")
	}
	if !strings.Contains(group.Label, "group") {
		t.Errorf("node group reads as %q — nothing tells the operator it is a container rather than a slot", group.Label)
	}
	if lane := byName["SUPERMARKET-L1"]; !strings.Contains(lane.Label, "lane") {
		t.Errorf("lane reads as %q — same problem, one level down", lane.Label)
	}
	if slot := byName["LINE1-IN"]; strings.Contains(slot.Label, "group") || strings.Contains(slot.Label, "lane") {
		t.Errorf("an ordinary slot is badged as a container: %q", slot.Label)
	}

	// The value the form posts is still the bare name / id — only the display
	// changed. A badge that leaked into the value would name a node that does
	// not exist.
	for _, o := range opts {
		if strings.ContainsAny(o.Name, "·↳") || strings.HasPrefix(o.Name, " ") {
			t.Errorf("option value %q carries display decoration; the form posts this string verbatim", o.Name)
		}
	}

	// Children sit under their parent, so the hierarchy is readable at a glance.
	var order []string
	for _, o := range opts {
		order = append(order, o.Name)
	}
	want := []string{"SUPERMARKET", "SUPERMARKET-L1", "SUPERMARKET-L1-S1", "LINE1-IN"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("picker order = %v, want %v — a child listed away from its parent reads as an unrelated place", order, want)
			break
		}
	}
}
