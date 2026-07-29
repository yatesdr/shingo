package www

import (
	"net/http"
	"time"

	"shingocore/domain"
)

// handlers_cycle_time.go — cycle time from the BinUOPDelta truth path (5.10).

// cycleWindow is how far back the distributions are computed over.
//
// NOT A DISPLAY CONSTANT in the provenance sense — it is a query scope, stated
// on the page in words, and no reader can mistake "the last 24 hours" for a
// claim that 24 hours is a healthy anything. The constants the binding rule
// covers are the ones that LOOK like knowledge.
//
// Twenty-four hours is chosen so a key on a 25 s takt accumulates thousands of
// gaps while a slow key on 75 s still clears the sample floor by an order of
// magnitude. A shorter window would put the small-n path on the majority of
// rows; a longer one would average over shift changes and weekend stops, which
// is the mean-of-a-heavy-tail mistake in a different costume.
const cycleWindow = 24 * time.Hour

// cycleRowLimit caps how many audit rows one page load reads.
//
// Sized against the real table: Springfield writes on the order of 7,000
// truth-path delta rows a day, so this is roughly three days of headroom on a
// 24-hour window. When it bites the page SAYS SO — see ListCycleEvents on why
// the cap narrows the window rather than punching holes in it, and why a
// distribution over a silently shortened window misreports its own n.
const cycleRowLimit = 20000

func (h *Handlers) handleCycleTime(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since := now.Add(-cycleWindow)
	c := h.engine.AppConfig().DisplayConstants()

	data := map[string]any{
		"Page":           "cycle-time",
		"WindowHours":    int(cycleWindow / time.Hour),
		"MinSamples":     c.CycleMinSamples,
		"SpreadMultiple": c.CycleSpreadMultiple,
		"BandWidth":      c.CycleBandWidth,
		"FlushInterval":  FormatDuration(c.CycleFlushInterval),
	}

	events, truncated, err := h.engine.AuditService().ListCycleEvents(since, cycleRowLimit)
	if err != nil {
		// THE ERROR IS SHOWN, NOT SWALLOWED INTO AN EMPTY TABLE. An empty table on
		// a query failure reads as "nothing is running", which is a claim about the
		// floor that a failed read is in no position to make.
		data["LoadError"] = err.Error()
		h.render(w, r, "cycle-time.html", data)
		return
	}

	series, unattributable := domain.BuildCycleSeries(events)

	rows := make([]CycleRow, 0, len(series))
	for _, s := range series {
		st := domain.SummarizeCycles(s, c.CycleMinSamples, c.CycleSpreadMultiple,
			c.CycleBandWidth, c.CycleFlushInterval)
		rows = append(rows, BuildCycleRow(st, c))
	}
	SortCycleRows(rows)

	data["Rows"] = rows
	data["Events"] = FormatCount(len(events))
	data["Truncated"] = truncated
	data["Limit"] = cycleRowLimit

	// Reported, never silently dropped. A delta row with no payload code cannot
	// be attributed to a part, so it cannot join any distribution — and how many
	// of those there are is a finding about the ingest path, not housekeeping.
	data["Unattributable"] = unattributable
	data["UnattributableText"] = FormatCount(unattributable)

	h.render(w, r, "cycle-time.html", data)
}
