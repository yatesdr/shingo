package robotconfidence

import (
	"math"
	"math/rand"
	"testing"
)

// THE PROPERTY THE WHOLE DESIGN RESTS ON: summing daily histograms and reading
// a percentile off the sum agrees with computing that percentile over the raw
// readings of every day at once.
//
// If this does not hold, the windowed board is lying, and it is lying in a way
// no reader could detect — which is precisely why it is asserted against a
// brute-force reference rather than against a hand-picked expectation.
//
// The tolerance is one bin width and that is not a fudge: it is the documented
// accuracy of the estimate, and a test that demanded exactness would be
// asserting something the structure cannot provide and does not claim to.
func TestHist_WindowSumAgreesWithRawPercentile(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(20260807))

	for trial := 0; trial < 200; trial++ {
		// Several "days", each with its own distribution, summed into a window.
		var window Hist
		var raw []float64
		days := 1 + rng.Intn(30)
		for d := 0; d < days; d++ {
			var day Hist
			n := rng.Intn(120)
			// Each day skews differently, so the window is not a single
			// distribution sampled repeatedly.
			lo := rng.Float64() * 0.6
			for i := 0; i < n; i++ {
				var v float64
				switch rng.Intn(6) {
				case 0:
					v = 0 // the sentinel, counted as the zero it is
				default:
					v = lo + rng.Float64()*(1-lo)
				}
				day.Add(v)
				raw = append(raw, math.Max(v, 0))
			}
			window.Merge(day)
		}
		if len(raw) == 0 {
			continue
		}

		for _, p := range []float64{0.05, 0.25, 0.50, 0.75, 0.95} {
			want := Percentile(raw, p) // the exact nearest-rank answer
			got, ok := window.PercentileEstimate(p)
			if !ok {
				t.Fatalf("trial %d: no estimate from %d readings", trial, len(raw))
			}
			if math.Abs(got-want) > HistBinWidth {
				t.Fatalf("trial %d p%.0f: estimate %.4f vs raw %.4f — off by %.4f, "+
					"more than one bin width (%.2f). n=%d days=%d",
					trial, p*100, got, want, math.Abs(got-want), HistBinWidth,
					len(raw), days)
			}
		}
		if window.Total() != len(raw) {
			t.Fatalf("trial %d: histogram holds %d, raw holds %d", trial, window.Total(), len(raw))
		}
	}
}

// The vendor's band edges must fall exactly on bin edges, or the banded counts
// disagree with the banded map.
func TestHist_VendorThresholdsAreOnBinEdges(t *testing.T) {
	t.Parallel()
	for _, edge := range []float64{0.30, 0.80} {
		q := edge / HistBinWidth
		if math.Abs(q-math.Round(q)) > 1e-9 {
			t.Errorf("band edge %.2f is not a bin edge at width %.2f — banded counts "+
				"would disagree with the banded map", edge, HistBinWidth)
		}
	}
}

// THE SENTINEL IS A POINT, NOT A RANGE, and a lane that produced nothing all
// day must band BLIND rather than "nearly blind".
//
// This is the trap the separate bin exists to prevent: fold the sentinel into
// the first value bin and interpolation reports ~0.01 for a lane with no
// estimate at all — which lands in the vendor's "> 0" band instead of "exactly
// 0", so the worst possible lane renders one step better than it is.
func TestHist_AllSentinelBandsExactlyZero(t *testing.T) {
	t.Parallel()
	var h Hist
	for i := 0; i < 40; i++ {
		h.Add(math.Copysign(0, -1)) // the real wire value; -0.0 is not a Go literal
	}
	got, ok := h.PercentileEstimate(0.50)
	if !ok {
		t.Fatal("no estimate from 40 sentinel readings")
	}
	if got != 0 {
		t.Errorf("p50 = %v, want exactly 0 — a lane whose every reading was a "+
			"no-estimate is blind, and any non-zero value bands it as working", got)
	}
	if h.SentinelCount() != 40 {
		t.Errorf("SentinelCount = %d, want 40", h.SentinelCount())
	}
}

// The two populations come out of one structure, which is the point of storing
// the sentinel separately rather than as a second column.
func TestHist_CarriesBothPopulations(t *testing.T) {
	t.Parallel()
	var h Hist
	for i := 0; i < 5; i++ {
		h.Add(math.Copysign(0, -1))
	}
	for i := 0; i < 5; i++ {
		h.Add(0.90)
	}

	all, _ := h.PercentileEstimate(0.50)
	if all > HistBinWidth {
		t.Errorf("all-ticks p50 = %v, want ~0 — half the readings were misses and "+
			"the banded statistic counts a miss as the zero it is", all)
	}
	good, ok := h.GoodOnly().PercentileEstimate(0.50)
	if !ok {
		t.Fatal("no estimate over the good population")
	}
	if math.Abs(good-0.90) > HistBinWidth {
		t.Errorf("good-ticks p50 = %v, want ~0.90 — the conditioned view, which "+
			"is what the panel shows and what must never be banded", good)
	}
	// The gap between those two IS the finding. If they ever agree by
	// construction, one of them has stopped being computed.
	if math.Abs(all-good) < 0.5 {
		t.Errorf("the two populations agree (%v vs %v); the split has collapsed", all, good)
	}
}

func TestHist_EmptyHasNoAnswer(t *testing.T) {
	t.Parallel()
	var h Hist
	if v, ok := h.PercentileEstimate(0.50); ok {
		t.Errorf("empty histogram answered %v — absent must be distinguishable "+
			"from zero, which is the whole no-data/zero rule", v)
	}
}

// A stored row of the wrong length is ABSENT, never padded.
//
// Rows written before this column existed are exactly that case, and a padded
// histogram would produce a confident percentile from a distribution nobody
// stored.
func TestHistFromSlice_WrongLengthIsAbsent(t *testing.T) {
	t.Parallel()
	for _, v := range [][]int32{nil, {}, make([]int32, HistLen-1), make([]int32, HistLen+1)} {
		if _, ok := HistFromSlice(v); ok {
			t.Errorf("len %d accepted; a partially-read histogram must read as absent", len(v))
		}
	}
	if _, ok := HistFromSlice(make([]int32, HistLen)); !ok {
		t.Error("a correctly-sized histogram was rejected")
	}
}

// 1.0 belongs in the top bin, not off the end.
func TestHist_UpperBoundLandsInTheTopBin(t *testing.T) {
	t.Parallel()
	var h Hist
	h.Add(1.0)
	if h[HistLen-1] != 1 {
		t.Errorf("1.0 landed in bin %v, want the top bin", h)
	}
	got, _ := h.PercentileEstimate(0.50)
	if got < 1.0-HistBinWidth || got > 1.0 {
		t.Errorf("p50 of a single 1.0 reading = %v, want within the top bin", got)
	}
}
