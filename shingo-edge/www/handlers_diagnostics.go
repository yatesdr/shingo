package www

import (
	"database/sql"
	"errors"
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
	entries, pager := logsPage(h.debugLog.Entries(subsystem), subsystem, requestedPage(r))
	data := map[string]any{
		"Page":           "logs",
		"Entries":        entries,
		"Pager":          pager,
		"Subsystems":     h.debugLog.Subsystems(),
		"Subsystem":      subsystem,
		"Recon":          summary,
		"ReconAnomalies": reconAnomalies,
		"Deadletters":    deadletters,
		"CoreLink":       h.diagnosticsCoreLink(),
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
	msg, err := h.engine.Reconciliation().GetOutboxMessage(id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && msg == nil) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no outbox message %d", id))
		return
	}
	// A delivered row is skipped by the requeue, so a replay would answer ok
	// and do nothing.
	if err == nil && msg.SentAt != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("already sent at %s — nothing to replay",
			msg.SentAt.UTC().Format(time.RFC3339)))
		return
	}
	if err == nil {
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
