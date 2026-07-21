package orders

import (
	"testing"

	"shingo/protocol"
)

// TestReleasableAtCore pins the Edge-side mirror of Core's release
// precondition (shingo-core/dispatch/complex_release.go: reject anything that
// is neither staged nor in_transit with "invalid_state").
//
// This table is the contract. If Core's accepted set ever changes, this test
// fails and the two sides get reconciled deliberately instead of drifting —
// which is exactly how the deferred-supply desync happened: Edge's guard
// admitted four statuses Core refuses, and then transitioned the Edge row
// anyway.
func TestReleasableAtCore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status protocol.Status
		want   bool
		why    string
	}{
		{StatusStaged, true, "the normal release: robot is holding at its wait step"},
		{StatusInTransit, true, "duplicate fan-out from consolidated release, and multi-wait re-release"},

		// The four that caused the divergence: Edge's manager guard admits
		// them, Core refuses them with invalid_state.
		{StatusQueued, false, "Core has not dispatched it; nothing to release against"},
		{StatusSourcing, false, "Core is still acquiring reservations"},
		{StatusDispatched, false, "handed to the fleet but not yet holding at a wait"},
		{StatusAcknowledged, false, "fleet intake ack only, pre-sourcing"},

		{StatusPending, false, "pre-dispatch"},
		{StatusSubmitted, false, "pre-dispatch"},
		{StatusReshuffling, false, "mid-reshuffle, not at a wait"},

		// Faulted is a no-op at Core rather than an error; skipping it on the
		// Edge is equivalent and saves the round trip.
		{StatusFaulted, false, "Core no-ops a faulted release; skip it here"},

		{StatusDelivered, false, "past the wait entirely"},
		{StatusConfirmed, false, "terminal"},
		{StatusCancelled, false, "terminal"},
		{StatusFailed, false, "terminal"},
		{StatusSkipped, false, "terminal"},
	}

	for _, c := range cases {
		if got := ReleasableAtCore(c.status); got != c.want {
			t.Errorf("ReleasableAtCore(%q) = %v, want %v — %s", c.status, got, c.want, c.why)
		}
	}
}

// TestReleasableAtCore_ImpliesNonTerminal guards the ordering assumption every
// call site makes: it checks IsTerminal first, then ReleasableAtCore. If a
// terminal status were ever releasable the two guards would disagree and the
// call sites would need reordering.
func TestReleasableAtCore_ImpliesNonTerminal(t *testing.T) {
	t.Parallel()

	all := []protocol.Status{
		StatusPending, StatusSourcing, StatusQueued, StatusSubmitted,
		StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged,
		StatusDelivered, StatusConfirmed, StatusCancelled, StatusFailed,
		StatusSkipped, StatusReshuffling, StatusFaulted,
	}
	for _, s := range all {
		if ReleasableAtCore(s) && IsTerminal(s) {
			t.Errorf("status %q is both releasable and terminal — call sites assume these are disjoint", s)
		}
	}
}
