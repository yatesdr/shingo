package www

import (
	"net/http"
	"strconv"
)

func (h *Handlers) apiListCMSTransactions(w http.ResponseWriter, r *http.Request) {
	limit := 100
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	nodeIDStr := r.URL.Query().Get("node_id")
	if nodeIDStr != "" {
		nodeID, err := strconv.ParseInt(nodeIDStr, 10, 64)
		if err != nil {
			h.jsonError(w, "invalid node_id", http.StatusBadRequest)
			return
		}
		txns, err := h.engine.DB().ListCMSTransactions(nodeID, limit, offset)
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.jsonOK(w, txns)
		return
	}

	txns, err := h.engine.DB().ListAllCMSTransactions(limit, offset)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, txns)
}
