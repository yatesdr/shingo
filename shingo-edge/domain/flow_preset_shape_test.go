package domain

import (
	"testing"

	"shingo/protocol"
)

// flow_preset_shape_test.go — the compare that decides drift, and the one rule
// that makes a preset a preset.
//
// DRIFT IS COMPUTED FROM TRUTH (SYNTH R-L1): collapse each member style's
// claims and compare the SHAPE against the preset's. Never from the stored
// version — `source_preset_id/version` on a claim row is provenance, and a
// row that says "I came from v2" is not evidence that it still looks like v2.
//
// THE SHAPE IS THE FLOW MINUS THE PART. A preset is a shape and the part is
// chosen when it is applied, so two flows that differ only in payload_code are
// the SAME shape — and a compare that counted the part would report every
// member of every preset as drifted the moment a second part used it, which is
// the whole point of having presets.

func shapeCell(node string, mode protocol.SwapMode) FlowCell {
	return FlowCell{
		CoreNodeName: node, Role: protocol.ClaimRoleConsume, SwapMode: mode,
		PayloadCode: "PIA27", PairedCoreNode: "PLN_02",
		InboundSource: "Supermarket Empty Totes", OutboundDestination: "Supermarket Area",
	}
}

func TestFlowShape_Compare(t *testing.T) {
	t.Parallel()
	base := []FlowCell{
		shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex),
		shapeCell("PLN_04", protocol.SwapModeTwoRobotPressIndex),
	}

	// payload-only difference: a different part on every cell.
	otherPart := []FlowCell{
		shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex),
		shapeCell("PLN_04", protocol.SwapModeTwoRobotPressIndex),
	}
	otherPart[0].PayloadCode = "PIA26"
	otherPart[1].PayloadCode = ""

	oneField := []FlowCell{
		shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex),
		shapeCell("PLN_04", protocol.SwapModeTwoRobotPressIndex),
	}
	oneField[1].OutboundDestination = "Empty Tote Return"

	twoFields := []FlowCell{
		shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex),
		shapeCell("PLN_04", protocol.SwapModeTwoRobotPressIndex),
	}
	twoFields[0].SwapMode = protocol.SwapModeTwoRobot
	twoFields[1].InboundSource = "Line Buffer"

	added := append([]FlowCell{}, base...)
	added = append(added, shapeCell("PLN_06", protocol.SwapModeTwoRobotPressIndex))

	removed := []FlowCell{shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex)}

	for _, tc := range []struct {
		name       string
		got        []FlowCell
		wantSame   bool
		wantFields []string
	}{
		{"the same flow", base, true, nil},
		{"the same shape, a different part", otherPart, true, nil},
		{"one field differs", oneField, false, []string{"PLN_04 · Outbound Destination"}},
		{"two fields differ, both reported, in cell order", twoFields, false,
			[]string{"PLN_01 · Swap Mode", "PLN_04 · Inbound Source"}},
		{"a position added", added, false, []string{"PLN_06 · not in the preset"}},
		{"a position removed", removed, false, []string{"PLN_04 · missing"}},
	} {
		same, fields := CompareFlowShape(base, tc.got)
		if same != tc.wantSame {
			t.Errorf("%s: same = %v, want %v (fields %v)", tc.name, same, tc.wantSame, fields)
		}
		if len(fields) != len(tc.wantFields) {
			t.Errorf("%s: fields = %v, want %v", tc.name, fields, tc.wantFields)
			continue
		}
		for i := range fields {
			if fields[i] != tc.wantFields[i] {
				t.Errorf("%s: field %d = %q, want %q", tc.name, i, fields[i], tc.wantFields[i])
			}
		}
	}
}

// TestFlowShape_KeyIsOrderIndependent: the cells of a flow arrive in whatever
// order the store returned them, and two flows with the same cells in a
// different order are the same flow.
func TestFlowShape_KeyIsOrderIndependent(t *testing.T) {
	t.Parallel()
	a := []FlowCell{
		shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex),
		shapeCell("PLN_04", protocol.SwapModeTwoRobotPressIndex),
	}
	b := []FlowCell{a[1], a[0]}
	if same, fields := CompareFlowShape(a, b); !same {
		t.Errorf("reordered cells read as drift: %v", fields)
	}
	if FlowShapeKey(a) != FlowShapeKey(b) {
		t.Error("FlowShapeKey depends on cell order — two orderings of one flow would be two candidate shapes")
	}
}

// TestFlowShape_KeyIgnoresThePart is the candidate grouping's load-bearing
// property: every style that runs one shape with a different part lands in ONE
// candidate, which is what makes the migration offer an offer and not a list
// of every style on the press.
func TestFlowShape_KeyIgnoresThePart(t *testing.T) {
	t.Parallel()
	a := []FlowCell{shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex)}
	b := []FlowCell{shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex)}
	b[0].PayloadCode = "SOMETHING-ELSE"
	if FlowShapeKey(a) != FlowShapeKey(b) {
		t.Error("FlowShapeKey counts the part; every part would be its own candidate shape")
	}
	// And a real difference still separates them.
	b[0].InboundStaging = "STG_09"
	if FlowShapeKey(a) == FlowShapeKey(b) {
		t.Error("FlowShapeKey missed a field that differs")
	}
}

// TestFlowShape_StripPayloadLeavesEverythingElse: what a preset is saved as.
func TestFlowShape_StripPayload(t *testing.T) {
	t.Parallel()
	in := []FlowCell{shapeCell("PLN_01", protocol.SwapModeTwoRobotPressIndex)}
	in[0].KeyRoute = []string{"LM167", "LM9"}
	in[0].Advanced = &FlowAdvanced{ReorderPoint: 500}
	out := StripFlowPayload(in)
	if out[0].PayloadCode != "" {
		t.Errorf("payload_code survived: %q", out[0].PayloadCode)
	}
	if in[0].PayloadCode == "" {
		t.Error("StripFlowPayload mutated its input; a caller's flow is not its to change")
	}
	if out[0].CoreNodeName != "PLN_01" || out[0].PairedCoreNode != "PLN_02" ||
		out[0].InboundSource != "Supermarket Empty Totes" || len(out[0].KeyRoute) != 2 {
		t.Errorf("StripFlowPayload dropped part of the shape: %+v", out[0])
	}
	// ADVANCED IS NOT PART OF A SHAPE. It is a policy an engineer set on a
	// position — a reorder point, an allowed list — and a preset that carried
	// one would apply a policy along with a shape, silently.
	if out[0].Advanced != nil {
		t.Error("StripFlowPayload kept the Advanced block; a preset is a shape, not a policy")
	}
}
