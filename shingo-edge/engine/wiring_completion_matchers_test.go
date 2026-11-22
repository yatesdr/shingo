package engine

import "testing"

// TestIsCapacityBlocked pins the dropoff-side recognition. The caller asks
// it after Core's code (handleNodeOrderFailed): a drop's failure coded
// no_bin or no_source_bin is cleared whatever its wording, so the raw
// shapes below are all it decides, including the "no available bin at "
// prefix an older Core's pickup wording also starts with.
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
		{"an older Core's pickup phrasing (same prefix)", "no available bin at pickup node(s) for order 42", true},
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
