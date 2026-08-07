package service

import "testing"

// bandFor is the map from a windowed estimate to what the page draws, and the
// two ends of it are the ones that matter.
func TestBandFor(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }

	for _, tc := range []struct {
		name    string
		p50     *float64
		samples int
		want    BoardBand
	}{
		{"vendor good edge", f(0.80), 100, BandGood},
		{"just under good", f(0.7999), 100, BandFair},
		{"vendor fair edge", f(0.30), 100, BandFair},
		{"just under fair", f(0.2999), 100, BandPoor},
		// EXACTLY ZERO IS ITS OWN BAND. Every reading was a no-estimate, so the
		// lane is blind — a different finding from "very poor", and the reason
		// the histogram's sentinel bin is never interpolated.
		{"blind", f(0), 100, BandBlind},
		// A lane nobody drove is NOT a band. Rendering it like a measured lane
		// is the failure the whole design removes; rendering it as absent reads
		// as fine, so it is banded nodata and stays on the map.
		{"no readings at all", nil, 0, BandNoData},
		// Below the minimum n the estimate exists and is not trustworthy.
		{"below min n", f(0.95), BoardMinSamples - 1, BandNoData},
		{"exactly min n", f(0.95), BoardMinSamples, BandGood},
	} {
		if got := bandFor(tc.p50, tc.samples); got != tc.want {
			t.Errorf("%s: bandFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}
