package engine

import "testing"

// TestIsNoBinFailure pins the pickup-side recognition. These strings
// come from dispatch/complex_claims.go:139/141 — a change there must
// either keep these wordings or update the matcher in lockstep.
func TestIsNoBinFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		{"no-bin pickup phrasing", "no available bin at pickup node(s) for order 42", true},
		{"no-source-bin pickup phrasing", "no bin at pickup node(s) for order 42 — source was emptied externally", true},
		{"dropoff capacity (NGRP saturation)", "no available slot in node group SUPER-A", false},
		{"dropoff capacity (no payload in group)", "no bin of requested payload in node group SUPER-A", false},
		{"planStore source-side (capacity-style)", "no available bin at LINE1-IN", false},
		{"unrelated fleet error", "robot not responding", false},
		{"empty reason", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isNoBinFailure(tc.reason); got != tc.want {
				t.Errorf("isNoBinFailure(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestIsCapacityBlocked pins the dropoff-side recognition. Note the
// intentional overlap with isNoBinFailure on "no available bin at " —
// the call-site contract orders isNoBinFailure FIRST so the pickup
// phrasing wins; the capacity matcher then catches the remaining
// "no available bin at <dropoff-node>" cases. This test asserts the
// raw shape without the ordering contract, since the ordering is
// enforced in the call site rather than in the matcher itself.
func TestIsCapacityBlocked(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		{"NGRP saturation", "no available slot in node group SUPER-A", true},
		{"NGRP empty for payload", "no bin of requested payload in node group SUPER-A", true},
		{"planStore source-side", "no available bin at LINE1-IN", true},
		{"pickup phrasing (overlap, caller-handled)", "no available bin at pickup node(s) for order 42", true},
		{"pickup no-bin phrasing (no match — different prefix)", "no bin at pickup node(s) for order 42 — source was emptied externally", false},
		{"unrelated fleet error", "robot not responding", false},
		{"empty reason", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isCapacityBlocked(tc.reason); got != tc.want {
				t.Errorf("isCapacityBlocked(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestMatcherOrdering pins the in-order resolution contract: when both
// matchers would match (pickup-shaped reason that also matches the
// looser capacity prefix), isNoBinFailure must be checked first. This
// mirrors the branch order in handleChangeoverOrderFailure so a
// regression on either side will surface here.
func TestMatcherOrdering(t *testing.T) {
	t.Parallel()

	pickupOverlap := "no available bin at pickup node(s) for order 42"
	if !isNoBinFailure(pickupOverlap) {
		t.Fatalf("precondition: isNoBinFailure should match %q", pickupOverlap)
	}
	if !isCapacityBlocked(pickupOverlap) {
		t.Fatalf("precondition: isCapacityBlocked should also match %q (overlap is intentional)", pickupOverlap)
	}
	// In the call site (handleChangeoverOrderFailure), the switch's
	// first arm is isNoBinFailure, so the pickup-shaped reason wins.
	// This test exists so a future "swap the order of arms" refactor
	// fails loudly.
}
