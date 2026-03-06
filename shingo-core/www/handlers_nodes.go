package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

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

type nodeInventoryItem struct {
	Bin     *store.Bin     `json:"bin"`
	Payload *store.Payload `json:"payload,omitempty"`
}

func (h *Handlers) apiNodePayloads(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	bins, err := h.engine.DB().ListBinsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	payloads, _ := h.engine.DB().ListPayloadsByNode(id)
	payloadByBin := make(map[int64]*store.Payload, len(payloads))
	for _, p := range payloads {
		if p.BinID != nil {
			payloadByBin[*p.BinID] = p
		}
	}

	items := make([]nodeInventoryItem, 0, len(bins))
	for _, b := range bins {
		items = append(items, nodeInventoryItem{
			Bin:     b,
			Payload: payloadByBin[b.ID],
		})
	}
	h.jsonOK(w, items)
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

	binTypesJSON, _ := json.Marshal(pd.BinTypes)
	edgesJSON, _ := json.Marshal(pd.Edges)

	data := map[string]any{
		"Page":           "nodes",
		"Nodes":          pd.Nodes,
		"Counts":         pd.Counts,
		"Zones":          pd.Zones,
		"NodeLabels":    pd.NodeLabels,
		"NodeInfo":       pd.NodeInfo,
		"MapGroups":      pd.MapGroups,
		"MapClassOrder":  []string{"ActionPoint", "ChargePoint", "LocationMark"},
		"BinTypes":       pd.BinTypes,
		"Edges":          pd.Edges,
		"ChildCounts":    pd.ChildCounts,
		"Depths":         pd.Depths,
		"BinTypesJSON":   string(binTypesJSON),
		"EdgesJSON":      string(edgesJSON),
	}
	h.render(w, r, "nodes.html", data)
}

func (h *Handlers) handleNodeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	node := &store.Node{
		Name:     r.FormValue("name"),
		Zone:     r.FormValue("zone"),
		Enabled:  r.FormValue("enabled") == "on",
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

	// Save station mode + assignments
	stationMode := r.FormValue("station_mode")
	if stationMode != "" {
		h.engine.DB().SetNodeProperty(node.ID, "station_mode", stationMode)
	}
	if stationMode == "specific" {
		h.engine.DB().SetNodeStations(node.ID, r.Form["stations"])
	} else {
		h.engine.DB().SetNodeStations(node.ID, nil)
	}

	// Save bin type mode + assignments
	binTypeMode := r.FormValue("bin_type_mode")
	if binTypeMode != "" {
		h.engine.DB().SetNodeProperty(node.ID, "bin_type_mode", binTypeMode)
	}
	if binTypeMode == "specific" {
		var ids []int64
		for _, s := range r.Form["bin_type_ids"] {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		h.engine.DB().SetNodeBinTypes(node.ID, ids)
	} else {
		h.engine.DB().SetNodeBinTypes(node.ID, nil)
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

	node.Name = r.FormValue("name")
	node.Zone = r.FormValue("zone")
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

	// Update station mode + assignments
	stationMode := r.FormValue("station_mode")
	h.engine.DB().SetNodeProperty(node.ID, "station_mode", stationMode)
	if stationMode == "specific" {
		h.engine.DB().SetNodeStations(node.ID, r.Form["stations"])
	} else {
		h.engine.DB().SetNodeStations(node.ID, nil)
	}

	// Update bin type mode + assignments
	binTypeMode := r.FormValue("bin_type_mode")
	h.engine.DB().SetNodeProperty(node.ID, "bin_type_mode", binTypeMode)
	if binTypeMode == "specific" {
		var binTypeIDs []int64
		for _, s := range r.Form["bin_type_ids"] {
			if sID, err := strconv.ParseInt(s, 10, 64); err == nil {
				binTypeIDs = append(binTypeIDs, sID)
			}
		}
		h.engine.DB().SetNodeBinTypes(node.ID, binTypeIDs)
	} else {
		h.engine.DB().SetNodeBinTypes(node.ID, nil)
	}

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

// apiNodeDetail returns extended node info (stations, blueprints, properties, children).
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
	binTypes, _ := h.engine.DB().ListBinTypesForNode(id)
	props, _ := h.engine.DB().ListNodeProperties(id)

	// Effective (inherited) values for child nodes
	effectiveStations, _ := h.engine.DB().GetEffectiveStations(id)
	effectiveBinTypes, _ := h.engine.DB().GetEffectiveBinTypes(id)

	// Mode properties
	binTypeMode := h.engine.DB().GetNodeProperty(id, "bin_type_mode")
	stationMode := h.engine.DB().GetNodeProperty(id, "station_mode")

	var children []*store.Node
	if node.IsSynthetic {
		children, _ = h.engine.DB().ListChildNodes(id)
	}

	h.jsonOK(w, map[string]any{
		"node":                  node,
		"stations":              stations,
		"bin_types":             binTypes,
		"properties":            props,
		"children":              children,
		"effective_stations":    effectiveStations,
		"effective_bin_types":   effectiveBinTypes,
		"bin_type_mode":         binTypeMode,
		"station_mode":          stationMode,
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

// apiGenerateTestNodes creates a representative set of test nodes for debugging.
func (h *Handlers) apiGenerateTestNodes(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	// Check if test nodes already exist.
	nodes, err := db.ListNodes()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, n := range nodes {
		if strings.HasPrefix(n.Name, "TEST-") {
			h.jsonError(w, "test nodes already exist — delete them first", http.StatusConflict)
			return
		}
	}

	type nodeDef struct {
		name string
		zone string
	}

	defs := []nodeDef{
		// Warehouse A — 6 storage nodes
		{"TEST-WH-A01", "Warehouse-A"},
		{"TEST-WH-A02", "Warehouse-A"},
		{"TEST-WH-A03", "Warehouse-A"},
		{"TEST-WH-A04", "Warehouse-A"},
		{"TEST-WH-A05", "Warehouse-A"},
		{"TEST-WH-A06", "Warehouse-A"},
		// Warehouse B — 4 storage nodes
		{"TEST-WH-B01", "Warehouse-B"},
		{"TEST-WH-B02", "Warehouse-B"},
		{"TEST-WH-B03", "Warehouse-B"},
		{"TEST-WH-B04", "Warehouse-B"},
		// Production — 3 line-side nodes
		{"TEST-LINE-1", "Production"},
		{"TEST-LINE-2", "Production"},
		{"TEST-LINE-3", "Production"},
		// Staging — 2 nodes
		{"TEST-STAGE-IN", "Staging"},
		{"TEST-STAGE-OUT", "Staging"},
	}

	created := 0
	for _, d := range defs {
		n := &store.Node{
			Name:    d.name,
			Zone:    d.zone,
			Enabled: true,
		}
		if err := db.CreateNode(n); err != nil {
			h.jsonError(w, fmt.Sprintf("creating %s: %v", d.name, err), http.StatusInternalServerError)
			return
		}
		created++
	}

	// Node group with lanes and slots.
	groupID, err := db.CreateNodeGroup("TEST-NGRP-1")
	if err != nil {
		h.jsonError(w, fmt.Sprintf("creating node group: %v", err), http.StatusInternalServerError)
		return
	}
	created++ // the synthetic group node

	// Two direct children on the group node.
	for _, name := range []string{"TEST-NGRP-1-D1", "TEST-NGRP-1-D2"} {
		child := &store.Node{
			Name:     name,
			Zone:     "Production",
			Enabled:  true,
			ParentID: &groupID,
		}
		if err := db.CreateNode(child); err != nil {
			h.jsonError(w, fmt.Sprintf("creating %s: %v", name, err), http.StatusInternalServerError)
			return
		}
		created++
	}

	// Two lanes, each with 4 slot nodes.
	for _, laneName := range []string{"TEST-LANE-A", "TEST-LANE-B"} {
		laneID, err := db.AddLane(groupID, laneName)
		if err != nil {
			h.jsonError(w, fmt.Sprintf("adding lane %s: %v", laneName, err), http.StatusInternalServerError)
			return
		}
		created++ // lane node

		for i := 1; i <= 4; i++ {
			slotName := fmt.Sprintf("%s-S%d", laneName, i)
			slot := &store.Node{
				Name:     slotName,
				Zone:     "Production",
				Enabled:  true,
			}
			if err := db.CreateNode(slot); err != nil {
				h.jsonError(w, fmt.Sprintf("creating %s: %v", slotName, err), http.StatusInternalServerError)
				return
			}
			if err := db.ReparentNode(slot.ID, &laneID, i); err != nil {
				h.jsonError(w, fmt.Sprintf("reparenting %s: %v", slotName, err), http.StatusInternalServerError)
				return
			}
			created++
		}
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		Action: "created",
	}})

	log.Printf("generated %d test nodes", created)
	h.jsonOK(w, map[string]any{"created": created})
}

// apiDeleteTestNodes removes all TEST- prefixed nodes.
func (h *Handlers) apiDeleteTestNodes(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	nodes, err := db.ListNodes()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	deleted := 0

	// First pass: delete node groups (cascades to lanes + children).
	for _, n := range nodes {
		if strings.HasPrefix(n.Name, "TEST-") && n.IsSynthetic && n.ParentID == nil {
			if err := db.DeleteNodeGroup(n.ID); err != nil {
				h.jsonError(w, fmt.Sprintf("deleting group %s: %v", n.Name, err), http.StatusInternalServerError)
				return
			}
			deleted++
		}
	}

	// Second pass: delete remaining standalone TEST- nodes.
	// Re-fetch since DeleteNodeGroup may have removed children.
	nodes, _ = db.ListNodes()
	for _, n := range nodes {
		if strings.HasPrefix(n.Name, "TEST-") {
			if err := db.DeleteNode(n.ID); err != nil {
				log.Printf("delete test node %s: %v", n.Name, err)
				continue
			}
			deleted++
		}
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		Action: "deleted",
	}})

	log.Printf("deleted %d test nodes", deleted)
	h.jsonOK(w, map[string]any{"deleted": deleted})
}

// apiSetNodeBinTypes replaces bin type assignments for a node.
func (h *Handlers) apiSetNodeBinTypes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID     int64   `json:"node_id"`
		BinTypeIDs []int64 `json:"bin_type_ids"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.NodeID == 0 {
		h.jsonError(w, "node_id is required", http.StatusBadRequest)
		return
	}
	if err := h.engine.DB().SetNodeBinTypes(req.NodeID, req.BinTypeIDs); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiGetNodeBinTypes returns bin types assigned to a node.
func (h *Handlers) apiGetNodeBinTypes(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	binTypes, err := h.engine.DB().ListBinTypesForNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, binTypes)
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

