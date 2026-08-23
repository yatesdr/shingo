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

// Seats are positional, not names: a selection has to survive a press being
// re-cabled or a style re-pairing it, and a stored node name would not.
func TestSeatCoreNode_ResolvesByPosition(t *testing.T) {
	t.Parallel()
	c := pressClaim()
	for _, tc := range []struct{ seat, want string }{
		{EvacSeatFront, "PRESS_A"},
		{EvacSeatPaired, "PRESS_B"},
		{EvacSeatSecond, "PRESS_C"},
		{"not-a-seat", ""},
	} {
		if got := SeatCoreNode(c, tc.seat); got != tc.want {
			t.Errorf("SeatCoreNode(%q) = %q, want %q", tc.seat, got, tc.want)
		}
	}
	if got := SeatCoreNode(nil, EvacSeatFront); got != "" {
		t.Errorf("SeatCoreNode(nil) = %q, want empty", got)
	}
}

func TestMarkedEvacSeatNodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		seats  []string
		second string
		want   string
	}{
		{"none marked is the standing default", nil, "PRESS_C", ""},
		{"one seat", []string{EvacSeatPaired}, "PRESS_C", "PRESS_B"},
		{"all three", []string{EvacSeatFront, EvacSeatPaired, EvacSeatSecond}, "PRESS_C", "PRESS_A,PRESS_B,PRESS_C"},
		// Front to back, whatever order the selection was stored in: the plan
		// the builder emits has to be deterministic.
		{"order is front-to-back, not selection order",
			[]string{EvacSeatSecond, EvacSeatFront}, "PRESS_C", "PRESS_A,PRESS_C"},
		// A 2-position press whose claim still carries "second" from a
		// 3-position past must plan ONE evacuation, not one and a phantom at
		// the empty string.
		{"a marked seat the layout does not have contributes nothing",
			[]string{EvacSeatFront, EvacSeatSecond}, "", "PRESS_A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := pressClaim()
			c.SecondPairedCoreNode = tc.second
			c.ChangeoverEvacSeats = tc.seats
			got := strings.Join(MarkedEvacSeatNodes(c), ",")
			if got != tc.want {
				t.Errorf("MarkedEvacSeatNodes = %q, want %q", got, tc.want)
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

// The seat vocabulary is front-to-back, the direction bins index. Rendering
// and the builder both depend on it.
func TestChangeoverEvacSeatKeys_IsFrontToBack(t *testing.T) {
	t.Parallel()
	got := strings.Join(ChangeoverEvacSeatKeys(), ",")
	if got != "front,paired,second" {
		t.Errorf("seat keys = %q, want front,paired,second", got)
	}
}
