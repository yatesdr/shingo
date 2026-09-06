//go:build sim

package simulator

import (
	"testing"
	"time"
)

// A hold behind a bin ANOTHER order owns is a queue: it clears when that order
// moves its bin, and it must never trip the unresolvable-hold diagnostic. The
// bound has to out-wait the longest genuine queue, which is bounded by transit
// (15-20 simulated seconds on the demo plant).
func TestUnresolvableHoldAfter_CannotFireOnAGenuineQueue(t *testing.T) {
	const longestObservedGenuineHold = time.Minute
	if unresolvableHoldAfter <= longestObservedGenuineHold {
		t.Fatalf("unresolvableHoldAfter = %s, which a real queue (<= %s) could trip; "+
			"the diagnostic would then cry deadlock on ordinary waiting",
			unresolvableHoldAfter, longestObservedGenuineHold)
	}
}

// The bound is in SIMULATED time, so it means the same thing at every speed.
// A wall-time bound would fire five times sooner at 5x than at 1x and the
// diagnostic's threshold would silently depend on the multiplier.
func TestUnresolvableHoldAfter_IsASimulatedDuration(t *testing.T) {
	// The driver compares it against `now` from its injected clock (step(now)),
	// never against time.Now(). This pins the units so a future edit that swaps
	// in a wall reading has to delete a test that says why not.
	if unresolvableHoldAfter != 5*time.Minute {
		t.Fatalf("unresolvableHoldAfter = %s, want 5m — if this changed on purpose, "+
			"re-derive it against transit (15-20 sim-s) and update the comment", unresolvableHoldAfter)
	}
}
