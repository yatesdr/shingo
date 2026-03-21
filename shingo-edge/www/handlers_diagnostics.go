package www

import (
	"net/http"
)

func (h *Handlers) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	subsystem := r.URL.Query().Get("subsystem")
	data := map[string]any{
		"Page":       "logs",
		"Entries":    h.debugLog.Entries(subsystem),
		"Subsystems": h.debugLog.Subsystems(),
		"Subsystem":  subsystem,
	}
	h.renderTemplate(w, r, "diagnostics.html", data)
}
