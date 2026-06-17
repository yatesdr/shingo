package www

import (
	"encoding/json"
	"net/http"
)

func (h *Handlers) handleManualOrder(w http.ResponseWriter, r *http.Request) {
	processNodes, _ := h.engine.ProcessService().ListNodes()
	coreNodes := h.engine.CoreNodes()
	// Carry node type through to the picker (manual-order.js) so a synthetic
	// container (NGRP "group" / LANE "lane") badges distinctly from a targetable
	// slot and dotted child slots ("Group.Slot") nest under it — the bare-name
	// list let operators pick a synthetic group (e.g. "Supermarket Area") and
	// dead-end. Mirrors the core orders.js picker.
	type coreNodeOpt struct {
		Name       string `json:"name"`
		NodeType   string `json:"node_type,omitempty"`
		ParentType string `json:"parent_node_type,omitempty"`
	}
	coreNodeOpts := make([]coreNodeOpt, 0, len(coreNodes))
	coreNodeNames := make([]string, 0, len(coreNodes))
	for name, info := range coreNodes {
		coreNodeOpts = append(coreNodeOpts, coreNodeOpt{Name: name, NodeType: info.NodeType, ParentType: info.ParentNodeType})
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
	coreNodesJSON, _ := json.Marshal(coreNodeOpts)

	data := map[string]any{
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
