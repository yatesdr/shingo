package www

import (
	"log"
	"net/http"
	"strconv"

	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/store"
)

func (h *Handlers) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.DB().ListNodes()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, nodes)
}

func (h *Handlers) apiNodePayloads(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	payloads, err := h.engine.DB().ListInstancesByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, payloads)
}

func (h *Handlers) apiNodeState(w http.ResponseWriter, r *http.Request) {
	states, err := h.engine.NodeState().GetAllNodeStates()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, states)
}

func (h *Handlers) apiScenePoints(w http.ResponseWriter, r *http.Request) {
	class := r.URL.Query().Get("class")
	area := r.URL.Query().Get("area")

	var (
		points []*store.ScenePoint
		err    error
	)
	switch {
	case class != "":
		points, err = h.engine.DB().ListScenePointsByClass(class)
	case area != "":
		points, err = h.engine.DB().ListScenePointsByArea(area)
	default:
		points, err = h.engine.DB().ListScenePoints()
	}
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, points)
}

func (h *Handlers) handleNodes(w http.ResponseWriter, r *http.Request) {
	pd, _ := h.engine.GetNodesPageData()

	data := map[string]any{
		"Page":           "nodes",
		"Nodes":          pd.Nodes,
		"Counts":         pd.Counts,
		"Zones":          pd.Zones,
		"NodeLabels":    pd.NodeLabels,
		"NodeInfo":       pd.NodeInfo,
		"MapGroups":      pd.MapGroups,
		"MapClassOrder":  []string{"ActionPoint", "ChargePoint", "LocationMark"},
		"NodeTypes":      pd.NodeTypes,
		"SyntheticNodes": pd.SyntheticNodes,
		"PayloadStyles":  pd.PayloadStyles,
		"Edges":          pd.Edges,
		"ChildCounts":    pd.ChildCounts,
	}
	h.render(w, r, "nodes.html", data)
}

func (h *Handlers) handleNodeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	capacity, _ := strconv.Atoi(r.FormValue("capacity"))
	node := &store.Node{
		Name:           r.FormValue("name"),
		VendorLocation: r.FormValue("vendor_location"),
		NodeType:       r.FormValue("node_type"),
		Zone:           r.FormValue("zone"),
		Capacity:       capacity,
		Enabled:        r.FormValue("enabled") == "on",
	}

	if ntID, err := strconv.ParseInt(r.FormValue("node_type_id"), 10, 64); err == nil && ntID > 0 {
		node.NodeTypeID = &ntID
	}
	if parentID, err := strconv.ParseInt(r.FormValue("parent_id"), 10, 64); err == nil && parentID > 0 {
		node.ParentID = &parentID
	}

	if err := h.engine.DB().CreateNode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Save station assignments
	if stations := r.Form["stations"]; len(stations) > 0 {
		h.engine.DB().SetNodeStations(node.ID, stations)
	}

	// Save payload style compatibility
	if sIDs := r.Form["style_ids"]; len(sIDs) > 0 {
		var ids []int64
		for _, s := range sIDs {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		h.engine.DB().SetNodePayloadStyles(node.ID, ids)
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: node.ID, NodeName: node.Name, Action: "created",
	}})

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleNodeUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return
	}

	node, err := h.engine.DB().GetNode(id)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}

	capacity, _ := strconv.Atoi(r.FormValue("capacity"))
	node.Name = r.FormValue("name")
	node.VendorLocation = r.FormValue("vendor_location")
	node.NodeType = r.FormValue("node_type")
	node.Zone = r.FormValue("zone")
	node.Capacity = capacity
	node.Enabled = r.FormValue("enabled") == "on"

	if ntID, err := strconv.ParseInt(r.FormValue("node_type_id"), 10, 64); err == nil && ntID > 0 {
		node.NodeTypeID = &ntID
	} else {
		node.NodeTypeID = nil
	}
	if parentID, err := strconv.ParseInt(r.FormValue("parent_id"), 10, 64); err == nil && parentID > 0 {
		node.ParentID = &parentID
	} else {
		node.ParentID = nil
	}

	if err := h.engine.DB().UpdateNode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Update station assignments
	stations := r.Form["stations"]
	h.engine.DB().SetNodeStations(node.ID, stations)

	// Update payload style compatibility
	var styleIDs []int64
	for _, s := range r.Form["style_ids"] {
		if sID, err := strconv.ParseInt(s, 10, 64); err == nil {
			styleIDs = append(styleIDs, sID)
		}
	}
	h.engine.DB().SetNodePayloadStyles(node.ID, styleIDs)

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: node.ID, NodeName: node.Name, Action: "updated",
	}})

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleNodeSyncFleet(w http.ResponseWriter, r *http.Request) {
	total, created, deleted, err := h.engine.SceneSync()
	if err != nil {
		log.Printf("node sync: %v", err)
	} else {
		log.Printf("node sync: %d scene points, created %d, deleted %d nodes", total, created, deleted)
	}
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleSceneSync(w http.ResponseWriter, r *http.Request) {
	syncer, ok := h.engine.Fleet().(fleet.SceneSyncer)
	if !ok {
		log.Printf("scene sync: fleet backend does not support scene sync")
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	areas, err := syncer.GetSceneAreas()
	if err != nil {
		log.Printf("scene sync: fleet error: %v", err)
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	total, locationSet := h.engine.SyncScenePoints(areas)
	h.engine.UpdateNodeZones(locationSet, false)
	log.Printf("scene sync: %d points synced", total)
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return
	}

	node, err := h.engine.DB().GetNode(id)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}

	if err := h.engine.DB().DeleteNode(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: id, NodeName: node.Name, Action: "deleted",
	}})

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) apiNodeOccupancy(w http.ResponseWriter, r *http.Request) {
	results, err := h.engine.GetNodeOccupancy()
	if err != nil {
		code := http.StatusInternalServerError
		if engine.IsFleetUnsupported(err) {
			code = http.StatusNotImplemented
		}
		h.jsonError(w, err.Error(), code)
		return
	}
	h.jsonOK(w, results)
}

func (h *Handlers) apiNodeTestOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromNodeID int64 `json:"from_node_id"`
		ToNodeID   int64 `json:"to_node_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	result, err := h.engine.CreateDirectOrder(engine.DirectOrderRequest{
		FromNodeID: req.FromNodeID,
		ToNodeID:   req.ToNodeID,
		StationID:  "core-node-test",
		Desc:       "node test order from nodes page",
	})
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"order_id": result.OrderID,
		"from":     result.FromNode,
		"to":       result.ToNode,
	})
}

// apiNodeDetail returns extended node info (stations, payload types, properties, children).
func (h *Handlers) apiNodeDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	node, err := h.engine.DB().GetNode(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}

	stations, _ := h.engine.DB().ListStationsForNode(id)
	payloadStyles, _ := h.engine.DB().ListPayloadStylesForNode(id)
	props, _ := h.engine.DB().ListNodeProperties(id)

	var children []*store.Node
	if node.IsSynthetic {
		children, _ = h.engine.DB().ListChildNodes(id)
	}

	h.jsonOK(w, map[string]any{
		"node":          node,
		"stations":      stations,
		"payload_styles": payloadStyles,
		"properties":    props,
		"children":      children,
	})
}

// apiNodePropertySet upserts a key-value property on a node.
func (h *Handlers) apiNodePropertySet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID int64  `json:"node_id"`
		Key    string `json:"key"`
		Value  string `json:"value"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.NodeID == 0 || req.Key == "" {
		h.jsonError(w, "node_id and key are required", http.StatusBadRequest)
		return
	}
	if err := h.engine.DB().SetNodeProperty(req.NodeID, req.Key, req.Value); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiNodePropertyDelete removes a property from a node.
func (h *Handlers) apiNodePropertyDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID int64  `json:"node_id"`
		Key    string `json:"key"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.NodeID == 0 || req.Key == "" {
		h.jsonError(w, "node_id and key are required", http.StatusBadRequest)
		return
	}
	if err := h.engine.DB().DeleteNodeProperty(req.NodeID, req.Key); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

