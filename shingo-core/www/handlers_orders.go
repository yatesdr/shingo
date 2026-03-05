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
		"FilterStatus": status,
	}
	h.render(w, r, "orders.html", data)
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
		"History": history,
	}
	h.render(w, r, "orders.html", data)
}

func (h *Handlers) apiTerminateOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID int64 `json:"order_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	actor := h.getUsername(r)
	if err := h.engine.TerminateOrder(req.OrderID, actor); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
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
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	order, err := h.engine.DB().GetOrder(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}
	h.jsonOK(w, order)
}

func (h *Handlers) apiSetOrderPriority(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID  int64 `json:"order_id"`
		Priority int   `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	order, err := h.engine.DB().GetOrder(req.OrderID)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Update fleet priority if order has a vendor ID
	if order.VendorOrderID != "" {
		if err := h.engine.Fleet().SetOrderPriority(order.VendorOrderID, req.Priority); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := h.engine.DB().UpdateOrderPriority(order.ID, req.Priority); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
