package www

import (
	"net/http"
	"strconv"
)

func (h *Handlers) handleBoard(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "board.html", map[string]any{"Page": "board"})
}

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
	orders, err := h.engine.GetActiveOrdersWithRobotLocation()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}
