package domain

import (
	"strings"
	"testing"
)

func markedClaim() *NodeClaim {
	return &NodeClaim{
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

func TestEvacNodeMarked(t *testing.T) {
	t.Parallel()
	c := markedClaim()
	c.ChangeoverEvacNodes = []string{"PRESS_B"}

	if !EvacNodeMarked(c, "PRESS_B") {
		t.Error("a marked node reads as unmarked")
	}
	if EvacNodeMarked(c, "PRESS_A") {
		t.Error("an unmarked node reads as marked")
	}
	if EvacNodeMarked(nil, "PRESS_B") || EvacNodeMarked(c, "") {
		t.Error("nil claim / empty node must be false, not a panic")
	}
}

// A cell with more nodes than a press has positions is the point of the model:
// nothing counts to three.
func TestMarkedEvacNodes_IsNotPressShaped(t *testing.T) {
	t.Parallel()
	c := &NodeClaim{CoreNodeName: "WELD_1", PairedCoreNode: "WELD_2"}
	c.ChangeoverEvacNodes = []string{"WELD_2"}
	if got := strings.Join(MarkedEvacNodes(c), ","); got != "WELD_2" {
		t.Errorf("MarkedEvacNodes = %q, want WELD_2 — a claim that names several nodes can mark "+
			"any subset of them, whatever kind of cell it is", got)
	}
}
