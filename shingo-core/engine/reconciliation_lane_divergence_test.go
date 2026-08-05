//go:build docker

package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestCheckLaneLockDivergence_ArmsTheTripwire.
//
// LaneLock.CheckDivergence was written when the lane-lock dual-write landed and
// then never called outside lane_lock_mirror_test.go — no production caller at
// all. So the mirror ran unwatched: a dig row deleted out from under a live hold,
// or leaked after one ended, produced no signal anywhere. That is not a
// hypothetical; the per-block early release deletes the dig row at the first
// unbury pickup of every reshuffle (see dispatch's TestDigRow_SurvivesChildPickup),
// and it has been doing so silently.
//
// This pins the wiring rather than the detection — detection is CheckDivergence's
// own test. What matters here is that something in production asks the question
// every reconciliation tick, and that the count reaches the log.
func TestCheckLaneLockDivergence_ArmsTheTripwire(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	var logged []string
	svc := newReconciliationService(db, func(format string, args ...any) {
		logged = append(logged, strings.TrimSpace(fmt.Sprintf(format, args...)))
	})

	// Unbound: a service built without the engine's late binding must not panic
	// and must report nothing. Every test constructing this service bare relies
	// on that, and so does any future caller that forgets to wire it.
	if n := svc.CheckLaneLockDivergence(); n != 0 {
		t.Fatalf("unbound divergence check = %d, want 0", n)
	}
	if len(logged) != 0 {
		t.Fatalf("unbound divergence check logged %q, want silence", logged)
	}

	// In sync: no warning. A tripwire that reports on healthy state is one
	// operators learn to scroll past.
	svc.checkLaneLockDivergence = func() int { return 0 }
	if n := svc.CheckLaneLockDivergence(); n != 0 {
		t.Fatalf("in-sync divergence check = %d, want 0", n)
	}
	if len(logged) != 0 {
		t.Fatalf("in-sync divergence check logged %q, want silence", logged)
	}

	// Diverged: the count reaches the log, at WARN, pointing at the per-lane
	// lines CheckDivergence itself emits.
	svc.checkLaneLockDivergence = func() int { return 3 }
	if n := svc.CheckLaneLockDivergence(); n != 3 {
		t.Fatalf("diverged check = %d, want 3", n)
	}
	if len(logged) != 1 {
		t.Fatalf("diverged check logged %d lines, want 1: %q", len(logged), logged)
	}
	if !strings.Contains(logged[0], "WARN") || !strings.Contains(logged[0], "3") {
		t.Errorf("divergence log line = %q, want it to carry WARN and the count", logged[0])
	}
}
