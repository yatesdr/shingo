// handlers_admin_replenishment.go — UOP-threshold replenishment page.
//
// Phase 1 ships a minimal page with three sections (per v5 brief):
//
//   1. Loader thresholds:   per-(loader, payload) replenish_uop_threshold.
//   2. Cell autoreorder:    per-claim reorder_point + AutoReorder toggle.
//   3. Diagnostics:         operator-visible recent autoreorder evals
//                           (Phase 1.5 buffer item — basic placeholder
//                           ships here; the full structured-log view is
//                           Phase 2).
//
// The page renders the rows server-side from the engine; the JS does
// inline edits and dispatches PUTs to handlers_api_replenishment.

package www

import (
	"net/http"
)

type replenishmentClaimRow struct {
	ClaimID      int64
	ProcessName  string
	StyleName    string
	StyleActive  bool
	NodeName     string
	PayloadCode  string
	ReorderPoint int
	Source       string
	AutoReorder  bool
	UOPCapacity  int
}

func (h *Handlers) handleReplenishment(w http.ResponseWriter, r *http.Request) {
	loaderRows, err := h.orchestration.ListLoaderThresholds()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Cell autoreorder rows — every claim across every style on every
	// process, not just the active style. Engineers configure reorder
	// points proactively (pre-changeover), so showing only the active
	// style hides 80% of the configurable surface. Active style is
	// flagged per row so the operator can tell what's running now.
	var claimRows []replenishmentClaimRow
	processList, _ := h.engine.ProcessService().List()
	for _, p := range processList {
		activeStyleID := int64(0)
		if p.ActiveStyleID != nil {
			activeStyleID = *p.ActiveStyleID
		}
		styles, _ := h.engine.StyleService().ListByProcess(p.ID)
		for _, s := range styles {
			claims, _ := h.engine.StyleService().ListClaims(s.ID)
			for _, c := range claims {
				source := c.ReorderPointSource
				if source == "" {
					source = "legacy"
				}
				claimRows = append(claimRows, replenishmentClaimRow{
					ClaimID:      c.ID,
					ProcessName:  p.Name,
					StyleName:    s.Name,
					StyleActive:  s.ID == activeStyleID,
					NodeName:     c.CoreNodeName,
					PayloadCode:  c.PayloadCode,
					ReorderPoint: c.ReorderPoint,
					Source:       source,
					AutoReorder:  c.AutoReorder,
					UOPCapacity:  c.UOPCapacity,
				})
			}
		}
	}

	data := map[string]any{
		"Page":       "replenishment",
		"LoaderRows": loaderRows,
		"ClaimRows":  claimRows,
	}
	h.renderTemplate(w, r, "replenishment.html", data)
}
