package robotconfidence

import "math"

// The confidence histogram — what makes a WINDOW answerable from the permanent
// record.
//
// PERCENTILES DO NOT RE-AGGREGATE. HISTOGRAMS DO. That one sentence is the
// whole reason this exists. Seven daily p50s cannot be combined into a weekly
// p50 — it is the same argument that made this design store five percentiles
// instead of one — so a board offering 24 h / 7 d / 30 d / since-last-change
// windows would have to re-run the Go-side snap over raw samples for every
// window. That is a batch job wearing a page load's clothes.
//
// AND FOR TWO OF THE FOUR WINDOWS IT IS NOT MERELY SLOW, IT IS IMPOSSIBLE. Raw
// is retained fourteen days. The 30-day window cannot be served at all, and
// "since this lane's last geometry change" exceeds fourteen days for any lane
// nobody has edited recently — which is most of them. A control whose label
// promises a window the data cannot cover is the failure this project has spent
// three rounds removing.
//
// Summing daily histograms element-wise gives the window's distribution
// exactly, and the windowed percentile is then a lookup. No raw, no snap, no
// re-attribution, and the answer stays available for as long as the aggregate
// does — which is forever.
//
// ── The bin layout, and why these edges ────────────────────────────────────
//
// The vendor's bands are >= 0.80, 0.30-0.80, > 0, and exactly 0. A histogram
// whose bin edges do not land on 0.80 and 0.30 would make the banded counts
// disagree with the banded map, so those two edges are the constraint. At width
// 0.02, 0.30 is edge 15 and 0.80 is edge 40 — both exact.
//
// THE SENTINEL GETS ITS OWN BIN AND IT IS NOT A RANGE. `confidence <= 0` is the
// vendor's "no estimate here", a real value with a meaning, and the map bands
// exactly-zero separately from >0. Folding it into the first value bin would
// let a lane that produced NOTHING all day interpolate to ~0.01 and render as
// "poor but working" instead of "blind". So index 0 is the sentinel count, it
// is never interpolated, and a rank landing in it reads exactly 0.
//
// That one structure then carries both populations the design keeps separate:
// the all-ticks distribution the map bands is the whole array, and the
// good-ticks distribution the panel shows is the array without index 0.
const (
	// HistBins is the number of VALUE bins over (0, 1].
	HistBins = 50
	// HistBinWidth is 0.02 — see the edge argument above.
	HistBinWidth = 1.0 / float64(HistBins)
	// HistLen is the stored length: the sentinel bin plus the value bins.
	HistLen = HistBins + 1
)

// Hist counts readings per confidence bin.
//
// Index 0 is the sentinel (no estimate). Index 1+j counts genuine readings in
// [j*HistBinWidth, (j+1)*HistBinWidth), with the top bin closed at 1.0.
//
// INT32, NOT INT16, AND THE PLAN SAID SMALLINT. Worked at the stated target:
// 40 robots on a 2-second poll is 1.728 M readings a day fleet-wide, and a zone
// row aggregates every reading taken inside it — a busy zone can hold several
// hundred thousand in one bin, where smallint tops out at 32,767. A silently
// wrapped count is a wrong histogram that still renders, which is worse than
// the 100 bytes it saves. Lane rows would have fitted; area rows would not, and
// one type for both is what keeps the merge arithmetic honest. The row cost is
// ~204 B against a ~194 B row, so the permanent record stays inside the
// under-100 MB/year budget the retention policy sets.
type Hist [HistLen]int32

// Add files one reading.
//
// The sentinel test is NoEstimate's, not a separate threshold: `<= 0` catches
// the negative zero the wire actually sends, and re-deriving that rule here is
// how the two drift apart.
func (h *Hist) Add(conf float64) {
	if conf <= 0 {
		h[0]++
		return
	}
	j := int(conf / HistBinWidth)
	if j >= HistBins {
		// 1.0 exactly, or anything above it the vendor ever sends, belongs in
		// the top bin rather than off the end.
		j = HistBins - 1
	}
	h[1+j]++
}

// Merge adds another histogram into this one. This is the window operation.
func (h *Hist) Merge(o Hist) {
	for i := range o {
		h[i] += o[i]
	}
}

// Total is how many readings the histogram holds, sentinel included.
func (h Hist) Total() int {
	n := 0
	for _, c := range h {
		n += int(c)
	}
	return n
}

// SentinelCount is the readings that produced no estimate at all.
func (h Hist) SentinelCount() int { return int(h[0]) }

// PercentileEstimate returns the p-th percentile over EVERY reading, counting a
// no-estimate as the zero it is, and reports whether there was anything to
// compute it from.
//
// ESTIMATE IS IN THE NAME ON PURPOSE. The daily percentiles stored beside this
// are nearest-rank: every one of them is a value some robot actually reported.
// This one cannot be — the histogram has thrown away which reading fell where
// inside a bin — so it is accurate to within HistBinWidth and it must never be
// presented as a measured reading. The two are not interchangeable and the name
// is the only thing stopping a caller from treating them as though they were.
//
// A rank landing in the sentinel bin returns exactly 0, never an interpolated
// value: the sentinel is a point, not a range, and the map bands exactly-zero
// as its own state. A lane whose every reading was a no-estimate must band
// blind, not "nearly blind".
func (h Hist) PercentileEstimate(p float64) (float64, bool) {
	total := h.Total()
	if total == 0 {
		return 0, false
	}
	// Nearest rank, matching Percentile's convention so the daily and windowed
	// figures answer the same question at the same rank.
	rank := int(math.Ceil(p*float64(total))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= total {
		rank = total - 1
	}
	cum := 0
	for i, c := range h {
		next := cum + int(c)
		if rank < next {
			if i == 0 {
				return 0, true // the sentinel is a point, not a range
			}
			lo := float64(i-1) * HistBinWidth
			// Interpolate across the bin by the rank's position within it, so
			// a window whose readings are concentrated at one end does not
			// report the bin's midpoint.
			within := float64(rank-cum) / float64(c)
			return lo + within*HistBinWidth, true
		}
		cum = next
	}
	return 1.0, true
}

// GoodOnly returns the histogram with the sentinel bin cleared — the
// CONDITIONED distribution, which the panel shows and which must never be
// banded. Kept as its own call so a caller has to say which population it
// wants rather than getting one by default.
func (h Hist) GoodOnly() Hist {
	h[0] = 0
	return h
}

// Slice renders the histogram for storage as an INTEGER[].
func (h Hist) Slice() []int32 {
	out := make([]int32, HistLen)
	copy(out, h[:])
	return out
}

// HistFromSlice rebuilds a histogram from a stored row.
//
// A row of the wrong length is treated as ABSENT rather than padded or
// truncated: a partially-read histogram would produce a plausible percentile
// from a distribution nobody stored, and "we cannot answer this window" is the
// honest response. Rows written before this column existed are exactly that
// case.
func HistFromSlice(v []int32) (Hist, bool) {
	var h Hist
	if len(v) != HistLen {
		return h, false
	}
	copy(h[:], v)
	return h, true
}
