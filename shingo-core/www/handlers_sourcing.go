package www

import "net/http"

// handleSourcing renders the Core sourcing page — what every process can change
// over to right now, by process → styles, plus the replenishment queue. It reads
// the gated monitor snapshot (never its own recompute); the audience is SCO doing
// intermittent checks, so it is plain server-rendered HTML with no live refresh.
func (h *Handlers) handleSourcing(w http.ResponseWriter, r *http.Request) {
	view, err := h.engine.SourceabilityPage()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, r, "sourcing.html", map[string]any{
		"Page": "sourcing",
		"View": view,
	})
}
