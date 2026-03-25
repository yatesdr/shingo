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

	// Build lightweight node list for JS dropdown merge logic.
	type edgeNode struct {
		ID   string `json:"id"`
		Desc string `json:"desc"`
	}
	edgeNodeList := make([]edgeNode, 0, len(processNodes))
	for _, pn := range processNodes {
		edgeNodeList = append(edgeNodeList, edgeNode{
			ID:   pn.CoreNodeName,
			Desc: pn.Name,
		})
	}
	nodesJSON, _ := json.Marshal(edgeNodeList)
	coreNodesJSON, _ := json.Marshal(coreNodeNames)

	data := map[string]interface{}{
		"Page":              "manual-order",
		"ProcessNodes":      processNodes,
		"CoreNodes":         coreNodeNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"NodesJSON":         string(nodesJSON),
		"CoreNodesJSON":     string(coreNodesJSON),
	}

	h.renderTemplate(w, r, "manual-order.html", data)
}
