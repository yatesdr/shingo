package domain

import (
	"strings"
	"testing"

	"shingo/protocol"
)

func markedClaim() *NodeClaim {
	return &NodeClaim{
		SwapMode:             protocol.SwapModeTwoRobotPressIndex,
		CoreNodeName:         "PRESS_A",
		PairedCoreNode:       "PRESS_B",
		SecondPairedCoreNode: "PRESS_C",
		OutboundDestination:  "MARKET",
	}
}

// THE MARKS NAME NODES. They used to name positions — "front"/"paired"/
// "second" — resolved against the claim's layout fields, which was an
// accommodation for a schema fact (an index-paired node has no claim row to
// carry a flag) dressed up as a concept. Clearing a node is a node operation,
// and everything downstream was already keyed by node.
func TestMarkedEvacNodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		marked []string
		second string
		want   string
	}{
		{"none marked is the standing default", nil, "PRESS_C", ""},
		{"one node", []string{"PRESS_B"}, "PRESS_C", "PRESS_B"},
		{"all three", []string{"PRESS_A", "PRESS_B", "PRESS_C"}, "PRESS_C", "PRESS_A,PRESS_B,PRESS_C"},
		// Selection order is preserved: the operator picked these, and there is
		// no longer a canonical front-to-back vocabulary to re-sort them into.
		// The plan is deterministic because the stored list is.
		{"selection order is preserved", []string{"PRESS_C", "PRESS_A"}, "PRESS_C", "PRESS_C,PRESS_A"},
		// A mark the layout no longer holds is DROPPED rather than returned as
		// a phantom, so a stale row still plans the clearances it can do. The
		// operator is told about it at save time, not here.
		{"a node this claim does not hold is dropped", []string{"PRESS_B", "PRESS_C"}, "", "PRESS_B"},
		{"an unrelated node is dropped", []string{"SOME_OTHER_NODE"}, "PRESS_C", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := markedClaim()
			c.SecondPairedCoreNode = tc.second
			c.ChangeoverEvacNodes = tc.marked
			if got := strings.Join(MarkedEvacNodes(c), ","); got != tc.want {
				t.Errorf("MarkedEvacNodes = %q, want %q", got, tc.want)
			}
		})
	}
	if got := MarkedEvacNodes(nil); got != nil {
		t.Errorf("MarkedEvacNodes(nil) = %v, want nil", got)
	}
}

// A tool change marks a line's own positions on every mode a robot serves:
// a single-node cell its node, a sequential claim its own line. The paired
// name a sequential claim carries is its partner's line, which the partner's
// claim marks, so marking it here is dropped (and refused at save).
func TestMarkedEvacNodes_EveryLineModeMarksItsOwnPositions(t *testing.T) {
	t.Parallel()
	single := &NodeClaim{SwapMode: protocol.SwapModeSingleRobot, CoreNodeName: "CELL_1", ChangeoverEvacNodes: []string{"CELL_1"}}
	if got := strings.Join(MarkedEvacNodes(single), ","); got != "CELL_1" {
		t.Errorf("a single-node cell marks %q, want its node CELL_1", got)
	}
	seq := &NodeClaim{SwapMode: protocol.SwapModeSequential, CoreNodeName: "SEQ_A", PairedCoreNode: "SEQ_B",
		ChangeoverEvacNodes: []string{"SEQ_A", "SEQ_B"}}
	if got := strings.Join(MarkedEvacNodes(seq), ","); got != "SEQ_A" {
		t.Errorf("a sequential claim marks %q, want its own line SEQ_A alone", got)
	}
}

// TestChangeoverSituation_EveryArm: the one decision the planner and the
// readiness card both read, arm by arm. A press whose positions are marked
// evacuates whatever its parts do, unless every marked position keeps its
// part lineside.
func TestChangeoverSituation_EveryArm(t *testing.T) {
	t.Parallel()
	claim := func(part string) *NodeClaim {
		return &NodeClaim{CoreNodeName: "N", PayloadCode: part, Role: "consume", SwapMode: "single_robot"}
	}
	press := func(part string, marks ...string) *NodeClaim {
		c := markedClaim()
		c.PayloadCode, c.Role, c.SwapMode, c.ChangeoverEvacNodes = part, "produce", "two_robot_press_index", marks
		return c
	}
	keep := press("A", "PRESS_A")
	keep.ChangeoverCarryoverDisposition = CarryoverKeepLineside
	for _, tc := range []struct {
		name     string
		from, to *NodeClaim
		want     Situation
	}{
		{"no claim either side", nil, nil, SituationUnchanged},
		{"the incoming style adds the node", nil, claim("A"), SituationAdd},
		{"the incoming style holds it empty", nil, claim(emptyPayload), SituationUnchanged},
		{"the outgoing style drops the node", claim("A"), nil, SituationDrop},
		{"the incoming style clears it", claim("A"), claim(emptyPayload), SituationDrop},
		{"empty on both sides", claim(emptyPayload), claim(emptyPayload), SituationUnchanged},
		{"empty before, a part after", claim(emptyPayload), claim("A"), SituationAdd},
		{"the same part", claim("A"), claim("A"), SituationUnchanged},
		{"another part", claim("A"), claim("B"), SituationSwap},
		{"a marked press, another part", press("A", "PRESS_A"), press("B"), SituationEvacuate},
		{"a marked press, the same part", press("A", "PRESS_A"), press("A"), SituationEvacuate},
		{"a marked press whose every marked part stays lineside", keep, press("A"), SituationUnchanged},
	} {
		if got := ChangeoverSituation(tc.from, tc.to); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}
