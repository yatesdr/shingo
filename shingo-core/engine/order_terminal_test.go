package engine

import (
	"testing"

	"shingo/protocol"

	"shingocore/store/orders"
)

// TestOrderIsTerminal_FailsClosed pins the arm that made this worth extracting.
//
// protocol.IsTerminal("") is TRUE — the empty string has no outgoing transitions
// — so every caller that reads a status straight off a row it did not verify is
// one bad load away from treating an unknown order as finished. Two callers in
// this package act on that answer irreversibly, and only one of them carried the
// check.
func TestOrderIsTerminal_FailsClosed(t *testing.T) {
	// The premise, asserted rather than assumed: if this ever stops being true the
	// predicate below is redundant and this test says so instead of quietly passing.
	if !protocol.IsTerminal("") {
		t.Fatal("protocol.IsTerminal(\"\") is no longer true — the fail-closed arm's whole reason is " +
			"that an unset status reads as terminal; re-check both call sites before removing it")
	}

	cases := []struct {
		name  string
		order *orders.Order
		want  bool
	}{
		{"confirmed", &orders.Order{ID: 1, Status: protocol.StatusConfirmed}, true},
		{"failed", &orders.Order{ID: 1, Status: protocol.StatusFailed}, true},
		{"cancelled", &orders.Order{ID: 1, Status: protocol.StatusCancelled}, true},
		// `delivered` has an outgoing edge to confirmed. It is NOT terminal, and the
		// completion handler's first firing depends on that.
		{"delivered is not terminal", &orders.Order{ID: 1, Status: protocol.StatusDelivered}, false},
		{"in_transit", &orders.Order{ID: 1, Status: protocol.StatusInTransit}, false},
		{"UNSET status", &orders.Order{ID: 1}, false},
		{"nil order", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orderIsTerminal(tc.order); got != tc.want {
				t.Errorf("orderIsTerminal = %v, want %v", got, tc.want)
			}
		})
	}
}
