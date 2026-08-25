package domain

import (
	"strings"
	"testing"
)

func pressClaim() *NodeClaim {
	return &NodeClaim{
		CoreNodeName:         "PRESS_A",
		PairedCoreNode:       "PRESS_B",
		SecondPairedCoreNode: "PRESS_C",
		OutboundDestination:  "MARKET",
	}
}

// Positions are positional, not names: a selection has to survive a press being
// re-cabled or a style re-pairing it, and a stored node name would not.
func TestPositionCoreNode_ResolvesByPosition(t *testing.T) {
	t.Parallel()
	c := pressClaim()
	for _, tc := range []struct{ position, want string }{
		{EvacPositionFront, "PRESS_A"},
		{EvacPositionPaired, "PRESS_B"},
		{EvacPositionSecond, "PRESS_C"},
		{"not-a-position", ""},
	} {
		if got := PositionCoreNode(c, tc.position); got != tc.want {
			t.Errorf("PositionCoreNode(%q) = %q, want %q", tc.position, got, tc.want)
		}
	}
	if got := PositionCoreNode(nil, EvacPositionFront); got != "" {
		t.Errorf("PositionCoreNode(nil) = %q, want empty", got)
	}
}

func TestMarkedEvacPositionNodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		positions []string
		second    string
		want      string
	}{
		{"none marked is the standing default", nil, "PRESS_C", ""},
		{"one position", []string{EvacPositionPaired}, "PRESS_C", "PRESS_B"},
		{"all three", []string{EvacPositionFront, EvacPositionPaired, EvacPositionSecond}, "PRESS_C", "PRESS_A,PRESS_B,PRESS_C"},
		// Front to back, whatever order the selection was stored in: the plan
		// the builder emits has to be deterministic.
		{"order is front-to-back, not selection order",
			[]string{EvacPositionSecond, EvacPositionFront}, "PRESS_C", "PRESS_A,PRESS_C"},
		// A 2-position press whose claim still carries "second" from a
		// 3-position past must plan ONE evacuation, not one and a phantom at
		// the empty string.
		{"a marked position the layout does not have contributes nothing",
			[]string{EvacPositionFront, EvacPositionSecond}, "", "PRESS_A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := pressClaim()
			c.SecondPairedCoreNode = tc.second
			c.ChangeoverEvacPositions = tc.positions
			got := strings.Join(MarkedEvacPositionNodes(c), ",")
			if got != tc.want {
				t.Errorf("MarkedEvacPositionNodes = %q, want %q", got, tc.want)
			}
		})
	}
}

// Blank means "unchanged from today" — that is the entire compatibility story
// for the field, so it is pinned rather than left to read off the one-liner.
func TestEvacDestinationFor_FallsBackToOutbound(t *testing.T) {
	t.Parallel()
	c := pressClaim()
	if got := EvacDestinationFor(c); got != "MARKET" {
		t.Errorf("blank ChangeoverEvacDestination must fall back to OutboundDestination; got %q", got)
	}
	c.ChangeoverEvacDestination = "TOOLING-BAY"
	if got := EvacDestinationFor(c); got != "TOOLING-BAY" {
		t.Errorf("a set ChangeoverEvacDestination wins; got %q", got)
	}
	if got := EvacDestinationFor(nil); got != "" {
		t.Errorf("EvacDestinationFor(nil) = %q, want empty", got)
	}
}

// The position vocabulary is front-to-back, the direction bins index. Rendering
// and the builder both depend on it.
func TestChangeoverEvacPositionKeys_IsFrontToBack(t *testing.T) {
	t.Parallel()
	got := strings.Join(ChangeoverEvacPositionKeys(), ",")
	if got != "front,paired,second" {
		t.Errorf("position keys = %q, want front,paired,second", got)
	}
}
