package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"shingocore/domain"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/service"
)

// parseNodeAssignments pulls the station + bin-type selections out of
// an HTTP form into the shape NodeService.ApplyAssignments expects.
func parseNodeAssignments(r *http.Request) service.NodeAssignments {
	a := service.NodeAssignments{
		StationMode: r.FormValue("station_mode"),
		Stations:    r.Form["stations"],
		BinTypeMode: r.FormValue("bin_type_mode"),
	}
	for _, s := range r.Form["bin_type_ids"] {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			a.BinTypeIDs = append(a.BinTypeIDs, id)
		}
	}
	return a
}

func (h *Handlers) apiListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.NodeService().ListNodes()
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
	bins, err := h.engine.NodeService().ListBinsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, bins)
}

func (h *Handlers) apiNodeState(w http.ResponseWriter, r *http.Request) {
	states, err := h.engine.NodeService().ListNodeStates()
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
		points []*domain.ScenePoint
		err    error
	)
	switch {
	case class != "":
		points, err = h.engine.NodeService().ListScenePointsByClass(class)
	case area != "":
		points, err = h.engine.NodeService().ListScenePointsByArea(area)
	default:
		points, err = h.engine.NodeService().ListScenePoints()
	}
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, points)
}

func (h *Handlers) handleNodes(w http.ResponseWriter, r *http.Request) {
	pd, err := getNodesPageData(&nodesPageDataAdapter{ns: h.engine.NodeService(), bs: h.engine.BinService()})
	if err != nil {
		log.Printf("nodes page: get page data: %v", err)
	}

	binTypesJSON, _ := json.Marshal(pd.BinTypes)
	edgesJSON, _ := json.Marshal(pd.Edges)

	data := map[string]any{
		"Page":           "nodes",
		"Nodes":          pd.Nodes,
		"Counts":         pd.Counts,
		"TileStates":     pd.TileStates,
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

	node := &domain.Node{
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

	if err := h.engine.NodeService().CreateNode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.engine.NodeService().ApplyAssignments(node.ID, parseNodeAssignments(r)); err != nil {
		log.Printf("node create: apply assignments for node %d: %v", node.ID, err)
	}

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
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

	node, err := h.engine.NodeService().GetNode(id)
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

	if err := h.engine.NodeService().UpdateNode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// NodeService.ApplyAssignments always writes the station + bin-type mode,
	// even when the form posts an empty mode — that matches the pre-refactor
	// update-path behavior where an empty mode was written through verbatim.
	a := parseNodeAssignments(r)
	if err := h.engine.NodeService().ApplyAssignments(node.ID, a); err != nil {
		log.Printf("node update: apply assignments for node %d: %v", node.ID, err)
	}

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: node.ID, NodeName: node.Name, Action: "updated",
	}})

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleNodeSyncFleet(w http.ResponseWriter, r *http.Request) {
	total, created, deleted, err := h.orchestration.SceneSync()
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
	total, locationSet := h.orchestration.SyncScenePoints(areas)
	h.orchestration.UpdateNodeZones(locationSet, false)
	log.Printf("scene sync: %d points synced", total)
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid node id", http.StatusBadRequest)
		return
	}

	node, err := h.engine.NodeService().GetNode(id)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}

	if err := h.engine.NodeService().DeleteNode(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
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

// apiNodeDetail returns extended node info (stations, payloads, properties, children).
func (h *Handlers) apiNodeDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	svc := h.engine.NodeService()

	node, err := svc.GetNode(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}

	stations, err := svc.ListStationsForNode(id)
	if err != nil {
		log.Printf("node detail: list stations for node %d: %v", id, err)
	}
	binTypes, err := svc.ListBinTypesForNode(id)
	if err != nil {
		log.Printf("node detail: list bin types for node %d: %v", id, err)
	}
	props, err := svc.ListNodeProperties(id)
	if err != nil {
		log.Printf("node detail: list properties for node %d: %v", id, err)
	}

	// Effective (inherited) values for child nodes
	effectiveStations, err := svc.GetEffectiveStations(id)
	if err != nil {
		log.Printf("node detail: effective stations for node %d: %v", id, err)
	}
	effectiveBinTypes, err := svc.GetEffectiveBinTypes(id)
	if err != nil {
		log.Printf("node detail: effective bin types for node %d: %v", id, err)
	}

	// Mode properties
	binTypeMode := svc.GetNodeProperty(id, "bin_type_mode")
	stationMode := svc.GetNodeProperty(id, "station_mode")

	var children []*domain.Node
	if node.IsSynthetic {
		children, err = svc.ListChildNodes(id)
		if err != nil {
			log.Printf("node detail: list children for node %d: %v", id, err)
		}
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
	if err := h.engine.NodeService().SetNodeProperty(req.NodeID, req.Key, req.Value); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiGenerateTestNodes creates a representative set of test nodes for debugging.
func (h *Handlers) apiGenerateTestNodes(w http.ResponseWriter, r *http.Request) {
	svc := h.engine.NodeService()

	// Check if test nodes already exist.
	nodeList, err := svc.ListNodes()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, n := range nodeList {
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
		n := &domain.Node{
			Name:    d.name,
			Zone:    d.zone,
			Enabled: true,
		}
		if err := svc.CreateNode(n); err != nil {
			h.jsonError(w, fmt.Sprintf("creating %s: %v", d.name, err), http.StatusInternalServerError)
			return
		}
		created++
	}

	// Node group with lanes and slots.
	groupID, err := svc.CreateNodeGroup("TEST-NGRP-1")
	if err != nil {
		h.jsonError(w, fmt.Sprintf("creating node group: %v", err), http.StatusInternalServerError)
		return
	}
	created++ // the synthetic group node

	// Two direct children on the group node.
	for _, name := range []string{"TEST-NGRP-1-D1", "TEST-NGRP-1-D2"} {
		child := &domain.Node{
			Name:     name,
			Zone:     "Production",
			Enabled:  true,
			ParentID: &groupID,
		}
		if err := svc.CreateNode(child); err != nil {
			h.jsonError(w, fmt.Sprintf("creating %s: %v", name, err), http.StatusInternalServerError)
			return
		}
		created++
	}

	// Two lanes, each with 4 slot nodes.
	for _, laneName := range []string{"TEST-LANE-A", "TEST-LANE-B"} {
		laneID, err := svc.AddLane(groupID, laneName)
		if err != nil {
			h.jsonError(w, fmt.Sprintf("adding lane %s: %v", laneName, err), http.StatusInternalServerError)
			return
		}
		created++ // lane node

		for i := 1; i <= 4; i++ {
			slotName := fmt.Sprintf("%s-S%d", laneName, i)
			slot := &domain.Node{
				Name:     slotName,
				Zone:     "Production",
				Enabled:  true,
			}
			if err := svc.CreateNode(slot); err != nil {
				h.jsonError(w, fmt.Sprintf("creating %s: %v", slotName, err), http.StatusInternalServerError)
				return
			}
			if err := svc.ReparentNode(slot.ID, &laneID, i); err != nil {
				h.jsonError(w, fmt.Sprintf("reparenting %s: %v", slotName, err), http.StatusInternalServerError)
				return
			}
			created++
		}
	}

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		Action: "created",
	}})

	log.Printf("generated %d test nodes", created)
	h.jsonOK(w, map[string]any{"created": created})
}

// apiDeleteTestNodes removes all TEST- prefixed nodes.
func (h *Handlers) apiDeleteTestNodes(w http.ResponseWriter, r *http.Request) {
	svc := h.engine.NodeService()

	nodes, err := svc.ListNodes()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	deleted := 0

	// First pass: delete node groups (cascades to lanes + children).
	for _, n := range nodes {
		if strings.HasPrefix(n.Name, "TEST-") && n.IsSynthetic && n.ParentID == nil {
			if err := svc.DeleteNodeGroup(n.ID); err != nil {
				h.jsonError(w, fmt.Sprintf("deleting group %s: %v", n.Name, err), http.StatusInternalServerError)
				return
			}
			deleted++
		}
	}

	// Second pass: delete remaining standalone TEST- nodes.
	// Re-fetch since DeleteNodeGroup may have removed children.
	nodes, _ = svc.ListNodes()
	for _, n := range nodes {
		if strings.HasPrefix(n.Name, "TEST-") {
			if err := svc.DeleteNode(n.ID); err != nil {
				log.Printf("delete test node %s: %v", n.Name, err)
				continue
			}
			deleted++
		}
	}

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
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
	if err := h.engine.NodeService().SetNodeBinTypes(req.NodeID, req.BinTypeIDs); err != nil {
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
	binTypes, err := h.engine.NodeService().ListBinTypesForNode(id)
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
	if err := h.engine.NodeService().DeleteNodeProperty(req.NodeID, req.Key); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}
