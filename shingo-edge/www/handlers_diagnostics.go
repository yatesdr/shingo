package www

import (
	"fmt"
	"net/http"
	"time"

	"shingo/protocol"
)

func (h *Handlers) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	subsystem := r.URL.Query().Get("subsystem")
	summary, _ := h.engine.Reconciliation().Summary()
	reconAnomalies, _ := h.engine.Reconciliation().ListAnomalies()
	deadletters, _ := h.engine.Reconciliation().ListDeadLetterOutbox(50)
	// Counter anomalies + ReportingPointMap feed the shared navbar bell
	// (header.html). The diagnostics page also surfaces reconciliation
	// anomalies in its body table — those live under a distinct key so
	// they don't collide with the navbar's expected shape.
	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]any{
		"Page":              "logs",
		"Entries":           h.debugLog.Entries(subsystem),
		"Subsystems":        h.debugLog.Subsystems(),
		"Subsystem":         subsystem,
		"Recon":             summary,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"ReconAnomalies":    reconAnomalies,
		"Deadletters":       deadletters,
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
	// REFUSE AN EXPIRED ENVELOPE. On 2026-08-22 two dead-lettered production
	// deltas were replayed here: the row got sent_at, the edge logged "published
	// outbox msg N", the dead-letter count fell by two — and Core's ingestor
	// discarded both because the envelopes had expired 23 hours earlier. Every
	// layer reported a recovery that had not happened.
	//
	// The exp stamp is fixed at enqueue time, so age is decided before the
	// button exists. Re-stamping it on replay would be a per-subject class
	// decision nobody has made, and for a snapshot subject it would be wrong.
	if msg, err := h.engine.Reconciliation().GetOutboxMessage(id); err == nil && msg != nil {
		if hdr, perr := protocol.ParseHeader(msg.Payload, []byte(h.engine.AppConfig().Messaging.SigningKey)); perr == nil && protocol.IsExpiredHeader(hdr) {
			age := time.Since(hdr.ExpiresAt).Round(time.Second)
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"expired at %s, %s ago — cannot replay; Core drops an expired envelope "+
					"before any handler runs, so this would report success and change nothing",
				hdr.ExpiresAt.UTC().Format(time.RFC3339), age))
			return
		}
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
