package domain

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// TestFlowReadiness_NilNilIsReady: two styles with no claims have nothing to
// be unready about, and the answer is nil rather than an empty slice so a
// caller's "any findings?" reads the same either way.
func TestFlowReadiness_NilNilIsReady(t *testing.T) {
	t.Parallel()
	if got := ValidateFlowChangeoverReadiness(nil, nil); got != nil {
		t.Fatalf("nil, nil: got %v, want nil", got)
	}
}

// TestFlowReadiness_MatchesPlannerRegistry: for every outgoing mode and a
// blank incoming claim on the same node with a different payload — a swap,
// the situation the planner consults its registry for — the findings are
// flowspec.Changeover's Required entries, side for side, field for field, in
// order. This is the same list requiredChangeoverFields produces
// (engine.TestFlowspecMatchesRequiredChangeoverFields pins that side), so the
// two agree by construction through the table.
func TestFlowReadiness_MatchesPlannerRegistry(t *testing.T) {
	t.Parallel()
	modes := append(flowspec.ChangeoverModes(), "", "typo")
	for _, mode := range modes {
		from := []NodeClaim{{CoreNodeName: "PRESS", SwapMode: mode, PayloadCode: "OLD"}}
		to := []NodeClaim{{CoreNodeName: "PRESS", PayloadCode: "NEW"}}
		got := ValidateFlowChangeoverReadiness(from, to)
		var want []NodeFinding
		for _, sf := range flowspec.RequiredSideFields(flowspec.Changeover(mode)) {
			want = append(want, NodeFinding{CoreNodeName: "PRESS", Side: sf.Side, Field: sf.Field, Severity: SeverityError})
		}
		if len(got) != len(want) {
			t.Errorf("mode %q: %d findings, want %d: %+v", mode, len(got), len(want), got)
			continue
		}
		for i := range want {
			g := got[i]
			g.Message = ""
			if g != want[i] {
				t.Errorf("mode %q finding %d = %+v, want %+v", mode, i, got[i], want[i])
			}
			if got[i].Message == "" {
				t.Errorf("mode %q finding %d has no message", mode, i)
			}
		}
	}
}

// TestFlowReadiness_ReadySwapHasNoFindings: populate every field the mode's
// builder reads, on the side it reads it from, and the pair is ready.
func TestFlowReadiness_ReadySwapHasNoFindings(t *testing.T) {
	t.Parallel()
	for _, mode := range flowspec.ChangeoverModes() {
		from := NodeClaim{CoreNodeName: "PRESS", SwapMode: mode, PayloadCode: "OLD"}
		to := NodeClaim{CoreNodeName: "PRESS", PayloadCode: "NEW"}
		for sf, n := range flowspec.Changeover(mode) {
			if n != flowspec.Required {
				continue
			}
			if sf.Side == flowspec.SideFrom {
				populateClaimField(&from, sf.Field)
			} else {
				populateClaimField(&to, sf.Field)
			}
		}
		if got := ValidateFlowChangeoverReadiness([]NodeClaim{from}, []NodeClaim{to}); len(got) != 0 {
			t.Errorf("mode %q: a ready pair has findings %+v", mode, got)
		}
	}
}

// TestFlowReadiness_OnlyOrderBuildingPairsAreChecked mirrors the arms of
// engine.DiffStyleClaims that reach the registry: an add, a drop, an unchanged
// node and an explicit clear build no per-mode order and so produce no
// finding even with every routing field blank; a same-payload node that
// evacuates does.
func TestFlowReadiness_OnlyOrderBuildingPairsAreChecked(t *testing.T) {
	t.Parallel()
	blank := func(name, payload string) NodeClaim {
		return NodeClaim{CoreNodeName: name, SwapMode: protocol.SwapModeSingleRobot, PayloadCode: payload}
	}
	cases := []struct {
		name     string
		from, to []NodeClaim
		want     int
	}{
		{"add: node only on the incoming side", nil, []NodeClaim{blank("N", "NEW")}, 0},
		{"drop: node only on the outgoing side", []NodeClaim{blank("N", "OLD")}, nil, 0},
		{"unchanged: same payload, no evacuation", []NodeClaim{blank("N", "SAME")}, []NodeClaim{blank("N", "SAME")}, 0},
		{"explicit clear on the incoming side", []NodeClaim{blank("N", "OLD")}, []NodeClaim{blank("N", "__empty__")}, 0},
		{"node was empty, now needs material", []NodeClaim{blank("N", "__empty__")}, []NodeClaim{blank("N", "NEW")}, 0},
		{"swap: different payload", []NodeClaim{blank("N", "OLD")}, []NodeClaim{blank("N", "NEW")}, 3},
		{"evacuate: same payload, outgoing claim evacuates", []NodeClaim{func() NodeClaim {
			c := blank("N", "SAME")
			c.EvacuateOnChangeover = true
			return c
		}()}, []NodeClaim{blank("N", "SAME")}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateFlowChangeoverReadiness(tc.from, tc.to)
			if len(got) != tc.want {
				t.Errorf("%d findings, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

// TestFlowReadiness_PairsByNodeNameInOrder: claims pair by CoreNodeName
// whatever order the slices arrive in, and findings come back sorted by node
// so two runs render the same list.
func TestFlowReadiness_PairsByNodeNameInOrder(t *testing.T) {
	t.Parallel()
	from := []NodeClaim{
		{CoreNodeName: "PLN_02", SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "A"},
		{CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "A", OutboundDestination: "SMN"},
	}
	to := []NodeClaim{
		{CoreNodeName: "PLN_01", PayloadCode: "B", InboundStaging: "STG"},
		{CoreNodeName: "PLN_02", PayloadCode: "B", InboundStaging: "STG"},
	}
	got := ValidateFlowChangeoverReadiness(from, to)
	want := []NodeFinding{{CoreNodeName: "PLN_02", Side: flowspec.SideFrom, Field: flowspec.OutboundDestination, Severity: SeverityError}}
	if len(got) != 1 {
		t.Fatalf("got %+v, want one finding on PLN_02", got)
	}
	got[0].Message = ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// Reversed input order, same answer.
	rev := ValidateFlowChangeoverReadiness([]NodeClaim{from[1], from[0]}, []NodeClaim{to[1], to[0]})
	rev[0].Message = ""
	if !reflect.DeepEqual(rev, want) {
		t.Errorf("reversed input: got %+v, want %+v", rev, want)
	}
}

// TestFlowPartsPlaced_TakenOffIsNotLost: a part the engineer took off the flow
// in this draft is not a part that lost its position, so it raises nothing; a
// part dropped WITHOUT being taken off raises exactly the finding it always
// has. A taken-off code the stored flow never ran is ignored.
func TestFlowPartsPlaced_TakenOffIsNotLost(t *testing.T) {
	t.Parallel()
	stored := []NodeClaim{
		{CoreNodeName: "PLN_01", PayloadCode: "PART-A"},
		{CoreNodeName: "PLN_02", PayloadCode: "PART-B"},
	}
	draft := []NodeClaim{{CoreNodeName: "PLN_01", PayloadCode: "PART-A"}}

	if got := ValidateFlowPartsPlaced(stored, draft, []string{"PART-B"}); got != nil {
		t.Errorf("PART-B was taken off this flow and still reads as lost: %+v", got)
	}
	if got := ValidateFlowPartsPlaced(stored, draft, []string{"PART-B", "PART-NEVER"}); got != nil {
		t.Errorf("a taken-off code the flow never ran made a finding: %+v", got)
	}

	want := []NodeFinding{{
		Side:     flowspec.SideTo,
		Field:    flowspec.PayloadCode,
		Severity: SeverityError,
		Message:  "1 part needs a position: PART-B",
	}}
	for _, takenOff := range [][]string{nil, {}, {"PART-NEVER"}, {"PART-A"}} {
		if got := ValidateFlowPartsPlaced(stored, draft, takenOff); !reflect.DeepEqual(got, want) {
			t.Errorf("taken off %q: got %+v, want the finding unchanged %+v", takenOff, got, want)
		}
	}

	// Two dropped, one taken off: the other is still named, alone.
	stored = append(stored, NodeClaim{CoreNodeName: "PLN_03", PayloadCode: "PART-C"})
	got := ValidateFlowPartsPlaced(stored, draft, []string{"PART-C"})
	if len(got) != 1 || got[0].Message != "1 part needs a position: PART-B" {
		t.Errorf("one of two dropped parts taken off: got %+v, want only PART-B named", got)
	}
}
