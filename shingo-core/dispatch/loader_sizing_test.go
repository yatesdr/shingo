package dispatch

import (
	"testing"

	"shingo/shared/loadervectors"
)

// TestBinsToReachThreshold_GoldenVectors runs Core's sizing against the shared
// vectors. The Edge's sizing runs against the same file, so the two cannot
// diverge without a failure here or there.
//
// Sizing is half of what parity means. Agreeing on which window a carrier goes
// to while disagreeing on how many carriers there are is not parity.
func TestBinsToReachThreshold_GoldenVectors(t *testing.T) {
	t.Parallel()
	v := loadervectors.MustLoad()

	for _, c := range v.Sizing {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			bins, outcome, _ := BinsToReachThreshold(c.Threshold, c.CurrentUOP, c.PerBinCapacity)
			if bins != c.WantBins {
				t.Errorf("bins = %d, want %d\nwhy this case exists: %s", bins, c.WantBins, c.Why)
			}
			if string(outcome) != c.WantOutcome {
				t.Errorf("outcome = %q, want %q\nwhy this case exists: %s", outcome, c.WantOutcome, c.Why)
			}
		})
	}
}

// TestBinsToReachThreshold_NeverNegativeOrAbsurd sweeps the input space around
// the vectors' fixed points. The vectors pin the cases a person thought of;
// this pins the two properties that must hold everywhere: the answer is never
// negative, and it never exceeds the gap itself (which it would if the rounding
// were wrong at capacity 1).
func TestBinsToReachThreshold_NeverNegativeOrAbsurd(t *testing.T) {
	t.Parallel()
	for threshold := -5; threshold <= 200; threshold += 7 {
		for current := -500; current <= 250; current += 37 {
			for capacity := -2; capacity <= 60; capacity += 6 {
				bins, outcome, _ := BinsToReachThreshold(threshold, current, capacity)
				if bins < 0 {
					t.Fatalf("negative answer: threshold=%d current=%d capacity=%d -> %d", threshold, current, capacity, bins)
				}
				if capacity <= 0 {
					if outcome != SizingNoCapacity || bins != 0 {
						t.Fatalf("capacity=%d must be refused: got %d/%s", capacity, bins, outcome)
					}
					continue
				}
				clamped := current
				if clamped < 0 {
					clamped = 0
				}
				gap := threshold - clamped
				if gap <= 0 {
					if outcome != SizingAtThreshold || bins != 0 {
						t.Fatalf("threshold=%d current=%d: gap %d wants nothing, got %d/%s", threshold, current, gap, bins, outcome)
					}
					continue
				}
				if bins > gap {
					t.Fatalf("threshold=%d current=%d capacity=%d: %d carriers for a gap of %d — more carriers than units",
						threshold, current, capacity, bins, gap)
				}
				if (bins-1)*capacity >= gap {
					t.Fatalf("threshold=%d current=%d capacity=%d: %d carriers, but %d would already cover a gap of %d — rounding up too far",
						threshold, current, capacity, bins, bins-1, gap)
				}
				if bins*capacity < gap {
					t.Fatalf("threshold=%d current=%d capacity=%d: %d carriers hold %d, short of a gap of %d",
						threshold, current, capacity, bins, bins*capacity, gap)
				}
			}
		}
	}
}
