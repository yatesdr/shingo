package www

import (
	"encoding/json"
	"net/http"
)

func (h *Handlers) handleManualOrder(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	processNodes, _ := db.ListProcessNodes()
	coreNodes := h.engine.CoreNodes()
	coreNodeNames := make([]string, 0, len(coreNodes))
	for name := range coreNodes {
		coreNodeNames = append(coreNodeNames, name)
	}
	anomalies, rpMap := loadAnomalyData(h)

	coreNodesJSON, _ := json.Marshal(coreNodeNames)

	data := map[string]interface{}{
		"Page":              "manual-order",
		"ProcessNodes":      processNodes,
		"CoreNodes":         coreNodeNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"CoreNodesJSON":     string(coreNodesJSON),
	}

	h.renderTemplate(w, r, "manual-order.html", data)
}
