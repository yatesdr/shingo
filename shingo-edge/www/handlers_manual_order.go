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
	// Group marks a member of a node group: the pickup and the delivery of an
	// order the API takes are roles where Core picks, and leave members out
	// (manual-order.js, production.js; the API refuses one, refusePicked).
	type coreNodeOpt struct {
		Name     string `json:"name"`
		NodeType string `json:"node_type,omitempty"`
		Group    string `json:"group,omitempty"`
	}
	kinds := h.coreNodeKinds()
	coreNodeOpts := make([]coreNodeOpt, 0, len(coreNodes))
	coreNodeNames := make([]string, 0, len(coreNodes))
	for name, info := range coreNodes {
		coreNodeOpts = append(coreNodeOpts, coreNodeOpt{Name: name, NodeType: info.NodeType, Group: kinds.GroupOf(name)})
		coreNodeNames = append(coreNodeNames, name)
	}

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
		"Page":          "manual-order",
		"ProcessNodes":  processNodes,
		"CoreNodes":     coreNodeNames,
		"NodesJSON":     string(nodesJSON),
		"CoreNodesJSON": string(coreNodesJSON),
	}

	h.renderTemplate(w, r, "manual-order.html", data)
}
