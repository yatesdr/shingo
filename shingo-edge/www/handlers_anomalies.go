// handlers_anomalies.go — counter-anomaly confirm/dismiss endpoints.

package www

import "net/http"

func (h *Handlers) apiConfirmAnomaly(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := parseID(r, "snapshotID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot ID")
		return
	}
	if err := h.engine.CounterService().ConfirmAnomaly(snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDismissAnomaly(w http.ResponseWriter, r *http.Request) {
	snapshotID, err := parseID(r, "snapshotID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot ID")
		return
	}
	if err := h.engine.CounterService().DismissAnomaly(snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
