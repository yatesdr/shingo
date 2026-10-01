package release

import (
	"testing"

	"shingo/protocol"
)

// TestReleasable pins the Edge's mirror of Core's release precondition
// (shingo-core/dispatch/complex_release.go refuses anything that is neither
// staged nor in_transit with invalid_state). If Core's accepted set changes,
// this fails and the two sides are reconciled deliberately instead of
// drifting: an Edge guard that admitted four statuses Core refuses is how the
// deferred-supply desync happened.
//
// in_transit stays releasable: with the echo (S3), Core applies a release for
// the wait the robot is driving to when it gets there, so the act sends it
// ahead rather than holding for the stage.
func TestReleasable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status protocol.Status
		want   bool
	}{
		{protocol.StatusStaged, true},
		{protocol.StatusInTransit, true},
		{protocol.StatusQueued, false},
		{protocol.StatusSourcing, false},
		{protocol.StatusDispatched, false},
		{protocol.StatusAcknowledged, false},
		{protocol.StatusPending, false},
		{protocol.StatusSubmitted, false},
		{protocol.StatusReshuffling, false},
		{protocol.StatusFaulted, false}, // Core no-ops a faulted release; skipping it saves the round trip
		{protocol.StatusDelivered, false},
		{protocol.StatusConfirmed, false},
		{protocol.StatusCancelled, false},
		{protocol.StatusFailed, false},
		{protocol.StatusSkipped, false},
	} {
		if got := Releasable(c.status); got != c.want {
			t.Errorf("Releasable(%q) = %v, want %v", c.status, got, c.want)
		}
		if Releasable(c.status) && protocol.IsTerminal(c.status) {
			t.Errorf("%q is both releasable and terminal; the plans check terminal first and assume the two are disjoint", c.status)
		}
	}
}
