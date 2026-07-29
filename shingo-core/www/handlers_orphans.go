package www

import (
	"net/http"
	"time"
)

// handlers_orphans.go — Stage 5.7, the orphan lane and its trend.
//
// AN ORPHAN is an order that should have carried a demand origin and did not
// (origin_class = 'orphan', migration 61). Findings are never deleted and never
// auto-attached; the bucket is the evidence a reconciliation gap existed.
//
// SEPARATE PAGE, NOT A PANEL ON /demand-episodes. The episode browser's row is
// an EPISODE; an orphan is by definition an order with no episode, so it has no
// row to sit on there. Bolting it into that page's chrome would put a table
// with a different grain under the same heading — the overloading the style
// guide already names as this codebase's worst.

// orphanWindow is how far back the trend reaches.
//
// NOT A DISPLAY CONSTANT in the provenance sense, on the same reasoning as
// episodeWindow: it is a query scope stated on the page in words, and nobody
// reads "the last 7 days" as a claim that seven days is normal or healthy. The
// BUCKET WIDTH inside it is a different matter and does carry provenance — it
// silently decides what the trend can resolve. See config.DisplayConfig.
//
// SEVEN DAYS RATHER THAN THE BROWSER'S TWENTY-FOUR HOURS, because this surface
// is the one whose value is a slope. A day of hourly buckets is 24 points and a
// week is 168; the alarm here is "the rate has been climbing", which a single
// day cannot show and which the plan says is the whole point of the view.
const orphanWindow = 7 * 24 * time.Hour

func (h *Handlers) handleOrphans(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	c := h.engine.AppConfig().DisplayConstants()
	since := now.Add(-orphanWindow)

	data := map[string]any{
		"Page":            "orphans",
		"WindowDays":      int(orphanWindow / (24 * time.Hour)),
		"BucketLabel":     FormatDuration(c.OrphanBucket),
		"MinBucketOrders": c.MinBucketOrders,
	}

	// ── The lane ─────────────────────────────────────────────────────────────
	sites, err := h.engine.DemandEpisodeService().OrphanSites()
	if err != nil {
		// SHOWN, NOT SWALLOWED. An empty lane is the single most reassuring
		// thing this page can say — "no orders lost their origin anywhere" —
		// and on the day the read breaks it is also the least true.
		data["LaneError"] = err.Error()
	} else {
		data["Lane"] = BuildOrphanLane(sites, now)
	}

	// ── The trend ────────────────────────────────────────────────────────────
	buckets, err := h.engine.DemandEpisodeService().OrphanTrend(since, c.OrphanBucket)
	if err != nil {
		// Same rule. A trend rendered from nothing is a flat line at zero, and
		// a flat line at zero is exactly what a healthy plant looks like.
		data["TrendError"] = err.Error()
	} else {
		data["Trend"] = BuildOrphanTrend(buckets, since, now, c)
	}

	h.render(w, r, "orphans.html", data)
}
