package www

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func (h *Handlers) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.DB().ListNodes()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, nodes)
}

func (h *Handlers) apiListOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	orders, err := h.engine.DB().ListOrders(status, limit)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}

func (h *Handlers) apiGetOrder(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	order, err := h.engine.DB().GetOrder(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}
	h.jsonOK(w, order)
}

func (h *Handlers) apiNodeState(w http.ResponseWriter, r *http.Request) {
	states, err := h.engine.NodeState().GetAllNodeStates()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, states)
}

func (h *Handlers) apiNodePayloads(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	payloads, err := h.engine.DB().ListPayloadsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, payloads)
}

func (h *Handlers) apiRobotsStatus(w http.ResponseWriter, r *http.Request) {
	robots, err := h.engine.RDSClient().GetRobotsStatus()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, robots)
}

func (h *Handlers) apiListPayloadTypes(w http.ResponseWriter, r *http.Request) {
	types, err := h.engine.DB().ListPayloadTypes()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, types)
}

func (h *Handlers) apiHealthCheck(w http.ResponseWriter, r *http.Request) {
	rdsOK := false
	if _, err := h.engine.RDSClient().Ping(); err == nil {
		rdsOK = true
	}
	h.jsonOK(w, map[string]any{
		"status":    "ok",
		"rds":       rdsOK,
		"messaging": h.engine.MsgClient().IsConnected(),
	})
}

func (h *Handlers) jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (h *Handlers) jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
