package www

import (
	"encoding/json"
	"net/http"
	"strconv"

	"shingocore/rds"
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

func (h *Handlers) apiTerminateOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID int64 `json:"order_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	order, err := h.engine.DB().GetOrder(req.OrderID)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Terminate RDS order if it has an RDS order ID
	if order.RDSOrderID != "" {
		_ = h.engine.RDSClient().TerminateOrder(&rds.TerminateRequest{
			ID: order.RDSOrderID,
		})
	}

	actor := h.getUsername(r)
	detail := "cancelled by " + actor
	if err := h.engine.DB().UpdateOrderStatus(order.ID, "cancelled", detail); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiSetOrderPriority(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID  int64 `json:"order_id"`
		Priority int   `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	order, err := h.engine.DB().GetOrder(req.OrderID)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Update RDS priority if order has an RDS ID
	if order.RDSOrderID != "" {
		if err := h.engine.RDSClient().SetPriority(order.RDSOrderID, req.Priority); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := h.engine.DB().UpdateOrderPriority(order.ID, req.Priority); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]string{"status": "ok"})
}
