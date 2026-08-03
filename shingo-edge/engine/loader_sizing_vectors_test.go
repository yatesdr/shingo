package engine

import (
	"testing"

	"shingo/shared/loadervectors"
)

// TestEdgeSizing_GoldenVectors runs the Edge's own sizing arithmetic against the
// shared vectors, so Core's ported version and the one the plant runs today are
// held to the same answers.
//
// The Edge's sizing is not a function — it is an inline block in
// HandleLoopBelowThreshold, and its per-bin-capacity guard sits in the caller
// above it. edgeSizing below reproduces it statement for statement, with the
// guard pulled in, and the comment on each step says which line of the original
// it stands for. That transcription is the weak point of this test, and it is
// deliberately kept trivial enough to check by eye: anything cleverer would be
// asserting that my rewrite matches my rewrite.
func TestEdgeSizing_GoldenVectors(t *testing.T) {
	t.Parallel()
	v := loadervectors.MustLoad()

	for _, c := range v.Sizing {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			bins, outcome := edgeSizing(c.Threshold, c.CurrentUOP, c.PerBinCapacity)
			if bins != c.WantBins {
				t.Errorf("bins = %d, want %d\nwhy this case exists: %s", bins, c.WantBins, c.Why)
			}
			if outcome != c.WantOutcome {
				t.Errorf("outcome = %q, want %q\nwhy this case exists: %s", outcome, c.WantOutcome, c.Why)
			}
		})
	}
}

// edgeSizing transcribes HandleLoopBelowThreshold's sizing.
//
//	operator_demand_loader.go:126 — the capacity guard, in the CALLER
//	operator_demand_loader.go:149-154 — the negative-current clamp
//	operator_demand_loader.go:159-164 — the gap and the at-threshold skip
//	operator_demand_loader.go:165 — desiredBins, rounding up
func edgeSizing(threshold, currentUOP, perBinCapacity int) (int, string) {
	if perBinCapacity <= 0 {
		return 0, "no_per_bin_capacity"
	}
	current := currentUOP
	if current < 0 {
		current = 0
	}
	gap := threshold - current
	if gap <= 0 {
		return 0, "at_threshold"
	}
	return (gap + perBinCapacity - 1) / perBinCapacity, "ok"
}
