package www

import (
	"net/http"
	"strconv"

	"shingocore/engine"
)

func (h *Handlers) apiCreateNodeGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		h.jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	id, err := h.engine.DB().CreateNodeGroup(req.Name)
	if err != nil {
		h.jsonError(w, "create node group: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: id, NodeName: req.Name, Action: "created",
	}})

	h.jsonOK(w, map[string]any{"id": id, "name": req.Name})
}

func (h *Handlers) apiAddLane(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupID int64  `json:"group_id"`
		Name    string `json:"name"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupID == 0 || req.Name == "" {
		h.jsonError(w, "group_id and name are required", http.StatusBadRequest)
		return
	}

	id, err := h.engine.DB().AddLane(req.GroupID, req.Name)
	if err != nil {
		h.jsonError(w, "add lane: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: id, NodeName: req.Name, Action: "created",
	}})

	h.jsonOK(w, map[string]any{"id": id, "name": req.Name})
}

func (h *Handlers) apiReparentNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID   int64  `json:"node_id"`
		ParentID *int64 `json:"parent_id"`
		Position int    `json:"position"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.NodeID == 0 {
		h.jsonError(w, "node_id is required", http.StatusBadRequest)
		return
	}

	// Validate node exists and is physical (non-synthetic)
	node, err := h.engine.DB().GetNode(req.NodeID)
	if err != nil {
		h.jsonError(w, "node not found", http.StatusNotFound)
		return
	}
	if node.IsSynthetic {
		h.jsonError(w, "cannot reparent synthetic nodes", http.StatusBadRequest)
		return
	}

	// Validate parent if set
	var parentIsGroup bool
	if req.ParentID != nil {
		parent, err := h.engine.DB().GetNode(*req.ParentID)
		if err != nil {
			h.jsonError(w, "parent not found", http.StatusNotFound)
			return
		}
		if parent.NodeTypeCode != "LANE" && parent.NodeTypeCode != "NGRP" {
			h.jsonError(w, "parent must be a lane or node group", http.StatusBadRequest)
			return
		}
		parentIsGroup = parent.NodeTypeCode == "NGRP"
	}

	// Track old parent for reindexing
	oldParentID := node.ParentID

	// When reparenting to NGRP, skip depth assignment (position=0)
	position := req.Position
	if parentIsGroup {
		position = 0
	}

	// Perform reparent
	if err := h.engine.DB().ReparentNode(req.NodeID, req.ParentID, position); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// On adopt: clear direct station and style assignments (now inherited)
	if req.ParentID != nil {
		h.engine.DB().SetNodeStations(req.NodeID, nil)
		h.engine.DB().SetNodePayloadStyles(req.NodeID, nil)
	}

	// Reindex siblings in new parent (only for lanes, not groups)
	if req.ParentID != nil && !parentIsGroup {
		h.reindexLaneSlots(*req.ParentID)
	}

	// Reindex siblings in old parent (if different)
	if oldParentID != nil && (req.ParentID == nil || *oldParentID != *req.ParentID) {
		h.reindexLaneSlots(*oldParentID)
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: req.NodeID, NodeName: node.Name, Action: "reparented",
	}})

	h.jsonSuccess(w)
}

// reindexLaneSlots recomputes depth for all children of a lane.
func (h *Handlers) reindexLaneSlots(laneID int64) {
	children, err := h.engine.DB().ListLaneSlots(laneID)
	if err != nil {
		return
	}
	var ids []int64
	for _, c := range children {
		ids = append(ids, c.ID)
	}
	h.engine.DB().ReorderLaneSlots(laneID, ids)
}

func (h *Handlers) apiReorderLaneSlots(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LaneID     int64   `json:"lane_id"`
		OrderedIDs []int64 `json:"ordered_ids"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.LaneID == 0 || len(req.OrderedIDs) == 0 {
		h.jsonError(w, "lane_id and ordered_ids are required", http.StatusBadRequest)
		return
	}
	lane, err := h.engine.DB().GetNode(req.LaneID)
	if err != nil {
		h.jsonError(w, "lane not found", http.StatusNotFound)
		return
	}
	if lane.NodeTypeCode != "LANE" {
		h.jsonError(w, "node is not a lane", http.StatusBadRequest)
		return
	}
	if err := h.engine.DB().ReorderLaneSlots(req.LaneID, req.OrderedIDs); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: req.LaneID, NodeName: lane.Name, Action: "reordered",
	}})
	h.jsonSuccess(w)
}

func (h *Handlers) apiGetGroupLayout(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	layout, err := h.engine.DB().GetGroupLayout(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, layout)
}

func (h *Handlers) apiDeleteNodeGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if err := h.engine.DB().DeleteNodeGroup(req.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: req.ID, Action: "deleted",
	}})

	h.jsonSuccess(w)
}
