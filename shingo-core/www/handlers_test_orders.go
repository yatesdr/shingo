// NOTE: despite the "_test_" in the filename, this is not a Go test file.
// It implements the operator-facing /test-orders admin page — a synthetic
// order testbench operators use to exercise the system via the web UI
// (UUID prefix "test-", station "core-test"). Rename to
// handlers_synthetic_orders.go is deferred to a dedicated PR: changing
// the URL path is a breaking contract for operator bookmarks and
// external automation, so that lives on its own with redirects.
//
// This file holds the page render and read-only list endpoints. Action
// endpoints live in handlers_test_orders_{kafka,direct,commands}.go.

package www

import (
	"net/http"
	"strconv"

	"shingocore/fleet"
)

// --- Test Orders Page ---

func (h *Handlers) handleTestOrders(w http.ResponseWriter, r *http.Request) {
	nodes, _ := h.engine.NodeService().ListNodes()
	payloads, _ := h.engine.PayloadService().List()
	data := map[string]any{
		"Page":     "test-orders",
		"Nodes":    nodes,
		"Payloads": payloads,
	}
	h.render(w, r, "test-orders.html", data)
}

func (h *Handlers) apiTestOrdersList(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.OrderService().ListOrdersByStation("core-test", 50)
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
	svc := h.engine.OrderService()
	order, err := svc.GetOrder(id)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	history, _ := svc.ListOrderHistory(id)
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
	points, err := h.engine.NodeService().ListScenePoints()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, points)
}
