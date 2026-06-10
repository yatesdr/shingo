package www

import (
	"net/http"
	"strings"
)

func (h *Handlers) handleDashboard(w http.ResponseWriter, r *http.Request) {
	activeOrders, _ := h.engine.OrderService().ListActiveOrders()
	nodes, _ := h.engine.NodeService().ListNodes()

	// Count orders by status
	statusCounts := map[string]int{}
	for _, o := range activeOrders {
		statusCounts[string(o.Status)]++
	}

	// Node stats
	enabledNodes := 0
	for _, n := range nodes {
		if n.Enabled {
			enabledNodes++
		}
	}

	// Fleet health check
	fleetOK := false
	if err := h.engine.Fleet().Ping(); err == nil {
		fleetOK = true
	}

	msgOK := h.engine.MsgClient().IsConnected()
	dbOK := h.engine.HealthService().PingDB() == nil
	recon, _ := h.engine.Reconciliation().Summary()

	trackerCount := 0
	if t := h.engine.Tracker(); t != nil {
		trackerCount = t.ActiveCount()
	}

	data := map[string]any{
		"Page":         "dashboard",
		"ActiveOrders": activeOrders,
		"StatusCounts": statusCounts,
		"TotalOrders":  len(activeOrders),
		"TotalNodes":   len(nodes),
		"EnabledNodes": enabledNodes,
		"FleetOK":      fleetOK,
		"FleetName":    h.engine.Fleet().Name(),
		"MessagingOK":  msgOK,
		"DatabaseOK":   dbOK,
		"PollerActive": trackerCount,
		"SSEClients":   h.eventHub.ClientCount(),
		"Recon":        recon,
	}
	h.render(w, r, "dashboard.html", data)
}

// apiEdgeReregister asks one edge (?station=) or every edge (omitted) to
// re-send its registration, refreshing the cell catalog on demand (Q-034) — the
// Dashboard "Re-sync edges" button. The round-trip is the existing
// edge.register_request over Kafka; the edge answers in a second or two with a
// fresh catalog that HandleEdgeRegister upserts. Auth-gated (it drives the fleet).
func (h *Handlers) apiEdgeReregister(w http.ResponseWriter, r *http.Request) {
	station := strings.TrimSpace(r.URL.Query().Get("station"))
	if err := h.engine.RequestEdgeReregister(station); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
