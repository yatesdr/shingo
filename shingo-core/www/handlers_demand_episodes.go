package www

import (
	"net/http"
	"time"
)

// handlers_demand_episodes.go — the demand browser (Stage 5.1, 5.4, 5.5, 5.6).
//
// PATH IS /demand-episodes, NOT /demand. The latter is the production-quota page
// and has been since long before the demand grain existed; two unrelated
// aggregates behind one word at two scales is the overloading the style guide
// already names as the worst in this codebase, and quietly taking the URL would
// have made it worse rather than better. Whether the two should be reconciled is
// an IA decision (Stage 7.6's shape), not a rename to make in passing.

// episodeWindow is how far back closed episodes are shown.
//
// NOT A DISPLAY CONSTANT in the provenance sense — it is a query scope, not a
// number rendered as a judgement about the floor, and a reader cannot mistake
// "the last 24 hours" for a claim that 24 hours is normal or healthy. The
// constants the binding rule covers are the ones that LOOK like knowledge;
// this one is stated on the page in words.
//
// Open episodes are never windowed out regardless of age — see
// ListDemandEpisodes. An episode open for six hours is the most important row
// the page can show, and a window on opened_at would drop it exactly as it
// became worth seeing.
const episodeWindow = 24 * time.Hour

// episodeLimit caps the page. See ListDemandEpisodes on why the cap is reported
// rather than silent.
const episodeLimit = 200

func (h *Handlers) handleDemandEpisodes(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since := now.Add(-episodeWindow)
	c := h.engine.AppConfig().DisplayConstants()

	data := map[string]any{
		"Page":         "demand-episodes",
		"WindowHours":  int(episodeWindow / time.Hour),
		"WorryAfter":   FormatDuration(c.WorryAfter),
		"ConcernAfter": FormatDuration(c.ConcernAfter),
		"MinExpected":  c.MinExpectedOrders,
	}

	episodes, truncated, err := h.engine.DemandEpisodeService().List(since, episodeLimit)
	if err != nil {
		// THE ERROR IS SHOWN, NOT SWALLOWED INTO AN EMPTY TABLE. An empty table
		// on a query failure reads as "no demand episodes", which is the most
		// reassuring thing this page can say and, on the day the read breaks,
		// the least true. Same family as rendering absence as zero.
		data["LoadError"] = err.Error()
		h.render(w, r, "demand-episodes.html", data)
		return
	}

	rows := make([]EpisodeRow, 0, len(episodes))
	for _, e := range episodes {
		rows = append(rows, BuildEpisodeRow(e, now, c))
	}
	SortRows(rows)

	// ── 5.2 — the child-outcome mix ──────────────────────────────────────────
	//
	// A SECOND QUERY, AND A FAILURE HERE IS NOT A FAILURE OF THE PAGE. The
	// episode list is worth rendering without the cause column; what is not
	// acceptable is rendering the cause column as though it had been read. `ok`
	// is false on error and AttachCauses puts every row's cause into no-data —
	// which is the truth, and looks nothing like the zero-order finding it
	// would otherwise be confused with.
	counts, causeErr := h.engine.DemandEpisodeService().ChildOutcomesSince(since)
	byOrigin := FoldChildCounts(counts)
	AttachCauses(rows, byOrigin, causeErr == nil)
	if causeErr != nil {
		data["CauseError"] = causeErr.Error()
	} else {
		data["CauseTotals"] = SummarizeCauses(byOrigin)
	}

	data["Rows"] = rows
	data["Truncated"] = truncated
	data["Limit"] = episodeLimit

	// 5.6 — the sweep's share of closes, as a visible number.
	closedBy, err := h.engine.DemandEpisodeService().ClosedBySince(since)
	if err != nil {
		// The list is still worth rendering without the summary. Say the summary
		// is missing rather than printing a share computed from nothing — a 0%
		// sweep share is exactly the reading a broken query would produce, and it
		// is the reassuring one.
		data["ClosedByError"] = err.Error()
	} else {
		data["ClosedBy"] = SummarizeClosedBy(closedBy)
	}

	h.render(w, r, "demand-episodes.html", data)
}
