package www

import (
	"net/http"
)

func (h *Handlers) handleDashboard(w http.ResponseWriter, r *http.Request) {
	activeOrders, _ := h.engine.DB().ListActiveOrders()
	nodes, _ := h.engine.DB().ListNodes()

	// Count orders by status
	statusCounts := map[string]int{}
	for _, o := range activeOrders {
		statusCounts[o.Status]++
	}

	// Node stats
	enabledNodes := 0
	for _, n := range nodes {
		if n.Enabled {
			enabledNodes++
		}
	}

	// RDS health check
	rdsOK := false
	if ping, err := h.engine.RDSClient().Ping(); err == nil && ping != nil {
		rdsOK = true
	}

	msgOK := h.engine.MsgClient().IsConnected()

	data := map[string]any{
		"Page":         "dashboard",
		"ActiveOrders": activeOrders,
		"StatusCounts": statusCounts,
		"TotalOrders":  len(activeOrders),
		"TotalNodes":   len(nodes),
		"EnabledNodes": enabledNodes,
		"RDSOK":        rdsOK,
		"MessagingOK":  msgOK,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "dashboard.html", data)
}
