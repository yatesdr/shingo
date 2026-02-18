package www

import (
	"net/http"
	"strconv"
)

func (h *Handlers) handleOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	orders, _ := h.engine.DB().ListOrders(status, limit)

	data := map[string]any{
		"Page":          "orders",
		"Orders":        orders,
		"FilterStatus":  status,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "orders.html", data)
}

func (h *Handlers) handleOrderDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	order, err := h.engine.DB().GetOrder(id)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	history, _ := h.engine.DB().ListOrderHistory(id)

	data := map[string]any{
		"Page":          "orders",
		"Order":         order,
		"History":       history,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "orders.html", data)
}
