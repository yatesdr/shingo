package www

import (
	"encoding/json"
	"net/http"
)

func (h *Handlers) handleManualOrder(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	slots, _ := db.ListSlots()
	nodes, _ := db.ListNodes()
	coreNodes := h.engine.CoreNodes()
	coreNodeNames := make([]string, 0, len(coreNodes))
	for name := range coreNodes {
		coreNodeNames = append(coreNodeNames, name)
	}
	anomalies, rpMap := loadAnomalyData(h)

	// JSON-encode for page-data attributes
	type nodeEntry struct {
		ID   string `json:"id"`
		Desc string `json:"desc"`
	}
	nodeEntries := make([]nodeEntry, 0, len(nodes))
	for _, n := range nodes {
		nodeEntries = append(nodeEntries, nodeEntry{ID: n.NodeID, Desc: n.Description})
	}
	nodesJSON, _ := json.Marshal(nodeEntries)
	coreNodesJSON, _ := json.Marshal(coreNodeNames)

	data := map[string]interface{}{
		"Page":              "manual-order",
		"Slots":             slots,
		"Nodes":             nodes,
		"CoreNodes":         coreNodeNames,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"NodesJSON":         string(nodesJSON),
		"CoreNodesJSON":     string(coreNodesJSON),
	}

	h.renderTemplate(w, r, "manual-order.html", data)
}
