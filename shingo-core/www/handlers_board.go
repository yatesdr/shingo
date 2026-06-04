package www

import (
	"net/http"
	"strconv"
)

func (h *Handlers) handleBoardOrders(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	if idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		order, err := h.engine.GetActiveOrderWithRobotLocation(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if order == nil {
			h.jsonOK(w, nil)
			return
		}
		h.jsonOK(w, order)
		return
	}

	// ?dashboard=N scopes the list to that dashboard's station set — the
	// server-side "area" filter. A dashboard with no stations is plant-wide.
	// Unknown id is a 404 rather than a silent plant-wide fallback, which
	// would mislead a misconfigured board into showing everything.
	if dashStr := r.URL.Query().Get("dashboard"); dashStr != "" {
		dashID, err := strconv.ParseInt(dashStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid dashboard", http.StatusBadRequest)
			return
		}
		d, err := h.engine.DashboardService().Get(dashID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if d == nil {
			http.Error(w, "dashboard not found", http.StatusNotFound)
			return
		}
		orders, err := h.engine.GetActiveOrdersWithRobotLocationFiltered(d.Stations)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.jsonOK(w, orders)
		return
	}

	orders, err := h.engine.GetActiveOrdersWithRobotLocation()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}
