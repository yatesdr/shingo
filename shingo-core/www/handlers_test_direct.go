package www

import (
	"net/http"

	"shingocore/engine"
)

func (h *Handlers) apiDirectOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromNodeID int64 `json:"from_node_id"`
		ToNodeID   int64 `json:"to_node_id"`
		Priority   int   `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	result, err := h.engine.CreateDirectOrder(engine.DirectOrderRequest{
		FromNodeID: req.FromNodeID,
		ToNodeID:   req.ToNodeID,
		StationID:  "core-direct",
		Priority:   req.Priority,
		Desc:       "direct test order from shingo core",
	})
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"order_id":        result.OrderID,
		"vendor_order_id": result.VendorOrderID,
		"from":            result.FromNode,
		"to":              result.ToNode,
	})
}

func (h *Handlers) apiDirectOrdersList(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.DB().ListOrdersByStation("core-direct", 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}
