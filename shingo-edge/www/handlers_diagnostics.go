package www

import (
	"fmt"
	"net/http"
)

func (h *Handlers) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	subsystem := r.URL.Query().Get("subsystem")
	summary, _ := h.engine.Reconciliation().Summary()
	anomalies, _ := h.engine.Reconciliation().ListAnomalies()
	deadletters, _ := h.engine.Reconciliation().ListDeadLetterOutbox(50)
	data := map[string]any{
		"Page":        "logs",
		"Entries":     h.debugLog.Entries(subsystem),
		"Subsystems":  h.debugLog.Subsystems(),
		"Subsystem":   subsystem,
		"Recon":       summary,
		"Anomalies":   anomalies,
		"Deadletters": deadletters,
	}
	h.renderTemplate(w, r, "diagnostics.html", data)
}

func (h *Handlers) apiReplayOutbox(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
		return
	}
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}
	if err := h.engine.Reconciliation().RequeueOutbox(id); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiRequestOrderStatusSync(w http.ResponseWriter, r *http.Request) {
	if err := h.engine.CoreSync().RequestOrderStatusSync(); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
