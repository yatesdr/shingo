package www

import (
	"encoding/json"
	"net/http"
)

func (h *Handlers) handleManualOrder(w http.ResponseWriter, r *http.Request) {
	processNodes, _ := h.engine.ProcessService().ListNodes()
	coreNodes := h.engine.CoreNodes()
	coreNodeNames := make([]string, 0, len(coreNodes))
	for name := range coreNodes {
		coreNodeNames = append(coreNodeNames, name)
	}
	edge := make([]manualEdgeNode, 0, len(processNodes))
	for _, pn := range processNodes {
		edge = append(edge, manualEdgeNode{ID: pn.CoreNodeName, Desc: pn.Name})
	}
	nodesJSON, coreNodesJSON := h.manualOrderNodes(edge)

	data := map[string]any{
		"Page":          "manual-order",
		"ProcessNodes":  processNodes,
		"CoreNodes":     coreNodeNames,
		"NodesJSON":     nodesJSON,
		"CoreNodesJSON": coreNodesJSON,
	}

	h.renderTemplate(w, r, "manual-order.html", data)
}

// manualOrderNodes is the node list the manual-order form (Manual order page,
// Production's copy) is rendered with, as JSON for its page data: Core's
// nodes, with each one's type and, for a member of a node group, its group
// (the rows /api/core-nodes answers with, less what the form does not read),
// and this Edge's process nodes by Core node name. The page reads the list
// once from here, and /api/core-nodes again on an SSE core-nodes frame
// (shared/node-picker.js). A member of a group is Core's to pick: the pickup
// greys it and the delivery offers it only for a complex order (the API
// refuses one, refusePicked).
// manualEdgeNode is one of this Edge's process nodes as the form reads it.
// The handler takes it, not the store's node type: www reaches the store only
// through the service interface (depguard www-no-direct-store).
type manualEdgeNode struct {
	ID   string `json:"id"`
	Desc string `json:"desc"`
}

func (h *Handlers) manualOrderNodes(edge []manualEdgeNode) (nodesJSON, coreNodesJSON string) {
	type coreNodeOpt struct {
		Name     string `json:"name"`
		NodeType string `json:"node_type,omitempty"`
		Group    string `json:"group,omitempty"`
	}
	kinds := h.coreNodeKinds()
	coreNodes := h.engine.CoreNodes()
	opts := make([]coreNodeOpt, 0, len(coreNodes))
	for name, info := range coreNodes {
		opts = append(opts, coreNodeOpt{Name: name, NodeType: info.NodeType, Group: kinds.GroupOf(name)})
	}
	if edge == nil {
		edge = []manualEdgeNode{}
	}
	n, _ := json.Marshal(edge)
	c, _ := json.Marshal(opts)
	return string(n), string(c)
}
