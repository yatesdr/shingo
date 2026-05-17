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

	"shingoedge/domain"
)

type replenishmentClaimRow struct {
	ClaimID      int64
	ProcessName  string
	StyleName    string
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

	// Cell autoreorder rows — every claim across active processes.
	// Read-through so the same page reflects changes made in /processes
	// without a separate sync step.
	var claimRows []replenishmentClaimRow
	processList, _ := h.engine.ProcessService().List()
	styleByID := map[int64]domain.Style{}
	if styles, err := h.engine.StyleService().List(); err == nil {
		for _, s := range styles {
			styleByID[s.ID] = s
		}
	}
	for _, p := range processList {
		if p.ActiveStyleID == nil {
			continue
		}
		styleName := ""
		if s, ok := styleByID[*p.ActiveStyleID]; ok {
			styleName = s.Name
		}
		claims, _ := h.engine.StyleService().ListClaims(*p.ActiveStyleID)
		for _, c := range claims {
			source := c.ReorderPointSource
			if source == "" {
				source = "legacy"
			}
			claimRows = append(claimRows, replenishmentClaimRow{
				ClaimID:      c.ID,
				ProcessName:  p.Name,
				StyleName:    styleName,
				NodeName:     c.CoreNodeName,
				PayloadCode:  c.PayloadCode,
				ReorderPoint: c.ReorderPoint,
				Source:       source,
				AutoReorder:  c.AutoReorder,
				UOPCapacity:  c.UOPCapacity,
			})
		}
	}

	data := map[string]interface{}{
		"Page":       "replenishment",
		"LoaderRows": loaderRows,
		"ClaimRows":  claimRows,
	}
	h.renderTemplate(w, r, "replenishment.html", data)
}
