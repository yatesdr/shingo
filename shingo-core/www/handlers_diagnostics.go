package www

import (
	"net/http"
)

func (h *Handlers) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	subsystem := r.URL.Query().Get("subsystem")
	data := map[string]any{
		"Page":          "logs",
		"Entries":       h.debugLog.Entries(subsystem),
		"Subsystems":    h.debugLog.Subsystems(),
		"Subsystem":  subsystem,
	}
	h.render(w, r, "diagnostics.html", data)
}

func (h *Handlers) apiHealthCheck(w http.ResponseWriter, r *http.Request) {
	fleetOK := false
	if err := h.engine.Fleet().Ping(); err == nil {
		fleetOK = true
	}
	dbOK := h.engine.DB().Ping() == nil
	h.jsonOK(w, map[string]any{
		"status":    "ok",
		"fleet":     fleetOK,
		"messaging": h.engine.MsgClient().IsConnected(),
		"database":  dbOK,
	})
}
