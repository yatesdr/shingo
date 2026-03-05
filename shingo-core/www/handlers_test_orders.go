package www

import (
	"net/http"
	"strconv"

	"shingocore/fleet"
)

func (h *Handlers) handleTestOrders(w http.ResponseWriter, r *http.Request) {
	nodes, _ := h.engine.DB().ListNodes()
	payloadStyles, _ := h.engine.DB().ListPayloadStyles()
	data := map[string]any{
		"Page":           "test-orders",
		"Nodes":          nodes,
		"PayloadStyles":  payloadStyles,
	}
	h.render(w, r, "test-orders.html", data)
}

func (h *Handlers) apiTestOrdersList(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.DB().ListOrdersByStation("core-test", 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}

func (h *Handlers) apiTestOrderDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	order, err := h.engine.DB().GetOrder(id)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	history, _ := h.engine.DB().ListOrderHistory(id)
	h.jsonOK(w, map[string]any{"order": order, "history": history})
}

func (h *Handlers) apiTestRobots(w http.ResponseWriter, r *http.Request) {
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot listing", http.StatusNotImplemented)
		return
	}
	robots, err := rl.GetRobotsStatus()
	if err != nil {
		h.jsonError(w, "fleet error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, robots)
}

func (h *Handlers) apiTestScenePoints(w http.ResponseWriter, r *http.Request) {
	points, err := h.engine.DB().ListScenePoints()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, points)
}
