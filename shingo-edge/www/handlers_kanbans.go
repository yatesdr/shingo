package www

import (
	"net/http"
	"strconv"

	"shingoedge/domain"
)

func (h *Handlers) handleKanbans(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ProcessService().List()

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

	var activeOrders []domain.Order
	if activeProcessID > 0 {
		activeOrders, _ = h.engine.OrderService().ListActiveByProcess(activeProcessID)
	} else {
		activeOrders, _ = h.engine.OrderService().ListActive()
	}

	// Core-synced nodes for redirect dropdown
	coreNodes := h.engine.CoreNodes()
	knownNodes := make([]string, 0, len(coreNodes))
	for name := range coreNodes {
		knownNodes = append(knownNodes, name)
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

func (h *Handlers) handleKanbansPartial(w http.ResponseWriter, r *http.Request) {
	var activeProcessID int64
	if p := r.URL.Query().Get("process"); p != "" {
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			activeProcessID = id
		}
	}
	var activeOrders []domain.Order
	if activeProcessID > 0 {
		activeOrders, _ = h.engine.OrderService().ListActiveByProcess(activeProcessID)
	} else {
		activeOrders, _ = h.engine.OrderService().ListActive()
	}
	data := map[string]interface{}{
		"ActiveOrders": activeOrders,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "orders-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
