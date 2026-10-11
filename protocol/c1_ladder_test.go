package protocol

import "testing"

// c1_ladder_test.go — the order ladder as C1 makes it (v4 §2.6; RULINGS-C1-P0
// F2, F4): pending → (acquiring) → dispatched, queued the one resting place short
// of the fleet, and `sourcing` no longer a status an order can be in.

// TestTransitions_NothingEntersSourcing (F4). `sourcing` keeps its key, so a row
// that still says it (an old Edge's, before its boot snapshot; a history row) is
// not terminal and can move on, and its outgoing edges are queued's. No edge
// enters it.
//
// RED at 647b65af: pending, queued, acknowledged and dispatched all reach it.
func TestTransitions_NothingEntersSourcing(t *testing.T) {
	for _, from := range AllStatuses() {
		if IsValidTransition(from, StatusSourcing) {
			t.Errorf("%s → sourcing is an edge; nothing may enter sourcing", from)
		}
	}
	if IsTerminal(StatusSourcing) {
		t.Fatal("sourcing became terminal: a row still saying it would freeze (DESIGN-C1 §7 F4)")
	}
	for _, to := range AllStatuses() {
		if IsValidTransition(StatusSourcing, to) != IsValidTransition(StatusQueued, to) {
			t.Errorf("sourcing → %s is %v and queued → %s is %v; sourcing's way out is queued's",
				to, IsValidTransition(StatusSourcing, to), to, IsValidTransition(StatusQueued, to))
		}
	}
}

// TestTransitions_TheLadderEdges: the edges C1 adds and removes.
//
// RED at 647b65af: dispatched → queued and reshuffling → pending are not edges;
// reshuffling → queued is.
func TestTransitions_TheLadderEdges(t *testing.T) {
	for _, c := range []struct {
		from, to Status
		want     bool
		why      string
	}{
		{StatusPending, StatusDispatched, true, "every order's first hand (C1.1.2)"},
		{StatusPending, StatusQueued, true, "the park"},
		{StatusQueued, StatusDispatched, true, "a parked order's hand"},
		{StatusDispatched, StatusQueued, true, "a fleet refusal queues under fleet_unavailable (C1.1.3)"},
		{StatusReshuffling, StatusPending, true, "a resumed or dissolved parent goes back to the start (F2)"},
		{StatusReshuffling, StatusQueued, false, "a resumed parent is not waiting on anything (F2)"},
		{StatusPending, StatusSkipped, true, "the skip door: a move whose named bin has gone"},
		{StatusQueued, StatusSkipped, true, "the skip door"},
	} {
		if got := IsValidTransition(c.from, c.to); got != c.want {
			t.Errorf("%s → %s is %v, want %v: %s", c.from, c.to, got, c.want, c.why)
		}
	}
}
