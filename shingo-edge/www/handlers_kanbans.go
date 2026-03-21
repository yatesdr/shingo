package www

import (
	"net/http"
	"strconv"

	"shingoedge/store"
)

func (h *Handlers) handleKanbans(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	processes, _ := db.ListProcesses()

	// Determine active process from query param (0 = all processes)
	var activeProcessID int64
	if lineParam := r.URL.Query().Get("process"); lineParam != "" {
		if id, err := strconv.ParseInt(lineParam, 10, 64); err == nil {
			// Validate process exists
			for _, l := range processes {
				if l.ID == id {
					activeProcessID = id
					break
				}
			}
		}
	}

	var activeOrders []store.Order
	if activeProcessID > 0 {
		activeOrders, _ = db.ListActiveOrdersByLine(activeProcessID)
	} else {
		activeOrders, _ = db.ListActiveOrders()
	}

	knownNodes, _ := db.ListKnownNodes()

	// Merge core-synced nodes into known nodes for redirect dropdown
	coreNodes := h.engine.CoreNodes()
	knownSet := make(map[string]bool, len(knownNodes))
	for _, n := range knownNodes {
		knownSet[n] = true
	}
	for name := range coreNodes {
		if !knownSet[name] {
			knownNodes = append(knownNodes, name)
		}
	}

	anomalies, rpMap := loadAnomalyData(h)

	data := map[string]interface{}{
		"Page":              "kanbans",
		"Processes":         processes,
		"ActiveProcessID":   activeProcessID,
		"ActiveOrders":      activeOrders,
		"KnownNodes":        knownNodes,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}

	h.renderTemplate(w, r, "kanbans.html", data)
}
