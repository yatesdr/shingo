package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"shingo/protocol"
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

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
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

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: id, NodeName: req.Name, Action: "created",
	}})

	h.jsonOK(w, map[string]any{"id": id, "name": req.Name})
}

func (h *Handlers) apiReparentNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID   int64  `json:"node_id"`
		ParentID *int64 `json:"parent_id"`
		Position int    `json:"position"`
		Force    bool   `json:"force"`
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

	// --- Reparent guard: check for orders that would break ---
	// NOTE: TOCTOU — a new order could arrive between this check and the
	// reparent below. Acceptable for rare, operator-initiated actions.
	if node.ParentID != nil {
		oldParent, gpErr := h.engine.DB().GetNode(*node.ParentID)
		if gpErr == nil && oldParent.NodeTypeCode == "NGRP" {
			blocked, bErr := h.engine.DB().ListActiveOrdersBySourceRef(
				[]string{oldParent.Name})
			if bErr != nil {
				h.jsonError(w, "failed to check active orders: "+bErr.Error(),
					http.StatusInternalServerError)
				return
			}
			if len(blocked) > 0 {
				if !req.Force {
					orderIDs := make([]int64, len(blocked))
					for i, o := range blocked {
						orderIDs[i] = o.ID
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict)
					json.NewEncoder(w).Encode(map[string]any{
						"error": fmt.Sprintf(
							"cannot reparent: %d active order(s) reference group %q as source",
							len(blocked), oldParent.Name),
						"order_ids": orderIDs,
					})
					return
				}
				// Force mode: fail blocked orders terminally
				for _, order := range blocked {
					_ = h.engine.DB().FailOrderAtomic(order.ID,
						fmt.Sprintf("source group %q restructured "+
							"(node %s reparented)", oldParent.Name, node.Name))
					// Populate EdgeUUID/StationID so the EventOrderFailed
					// handler's notification gate can route the message to
					// Edge. Without these fields the handler silently skips
					// the notification and Edge keeps showing the order as
					// active.
					h.engine.EventBus().Emit(engine.Event{
						Type: engine.EventOrderFailed,
						Payload: engine.OrderFailedEvent{
							OrderID:   order.ID,
							EdgeUUID:  order.EdgeUUID,
							StationID: order.StationID,
							ErrorCode: "group_restructured",
							Detail:    "source group restructured (node reparented)",
						},
					})
				}
			}
		}
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
		h.engine.DB().SetNodePayloads(req.NodeID, nil)
	}

	// Reindex siblings in new parent (only for lanes, not groups)
	if req.ParentID != nil && !parentIsGroup {
		h.reindexLaneSlots(*req.ParentID)
	}

	// Reindex siblings in old parent (if different)
	if oldParentID != nil && (req.ParentID == nil || *oldParentID != *req.ParentID) {
		h.reindexLaneSlots(*oldParentID)
	}

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: req.NodeID, NodeName: node.Name, Action: "reparented",
	}})

	// Notify Edge of structural change when old or new parent is an NGRP
	if node.ParentID != nil || parentIsGroup {
		oldWasNGRP := false
		if node.ParentID != nil {
			if op, e := h.engine.DB().GetNode(*node.ParentID); e == nil {
				oldWasNGRP = op.NodeTypeCode == "NGRP"
			}
		}
		if oldWasNGRP || parentIsGroup {
			h.engine.SendDataToEdge(
				protocol.SubjectNodeStructureChanged,
				protocol.StationBroadcast,
				&protocol.NodeStructureChanged{
					NodeID:      req.NodeID,
					NodeName:    node.Name,
					OldParentID: node.ParentID,
					NewParentID: req.ParentID,
					Action:      "reparented",
				},
			)
		}
	}

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
	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
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
		ID    int64 `json:"id"`
		Force bool  `json:"force"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	// Look up the group so we can check its type and name
	group, err := h.engine.DB().GetNode(req.ID)
	if err != nil {
		h.jsonError(w, "group not found", http.StatusNotFound)
		return
	}

	// Guard: only NGRP names can appear as source_node references.
	// LANEs and other types don't need the order check.
	if group.NodeTypeCode == "NGRP" {
		blocked, bErr := h.engine.DB().ListActiveOrdersBySourceRef([]string{group.Name})
		if bErr != nil {
			h.jsonError(w, "failed to check active orders: "+bErr.Error(),
				http.StatusInternalServerError)
			return
		}
		if len(blocked) > 0 {
			if !req.Force {
				orderIDs := make([]int64, len(blocked))
				for i, o := range blocked {
					orderIDs[i] = o.ID
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{
					"error": fmt.Sprintf(
						"cannot delete group: %d active order(s) reference %q as source",
						len(blocked), group.Name),
					"order_ids": orderIDs,
				})
				return
			}
			for _, order := range blocked {
				_ = h.engine.DB().FailOrderAtomic(order.ID,
					fmt.Sprintf("source group %q deleted", group.Name))
				// Populate EdgeUUID/StationID so the EventOrderFailed handler's
				// notification gate routes the message to Edge.
				h.engine.EventBus().Emit(engine.Event{
					Type: engine.EventOrderFailed,
					Payload: engine.OrderFailedEvent{
						OrderID:   order.ID,
						EdgeUUID:  order.EdgeUUID,
						StationID: order.StationID,
						ErrorCode: "group_deleted",
						Detail:    "source group deleted",
					},
				})
			}
		}
	}

	if err := h.engine.DB().DeleteNodeGroup(req.ID); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: req.ID, Action: "deleted",
	}})

	// Notify Edge of group deletion
	if group.NodeTypeCode == "NGRP" {
		h.engine.SendDataToEdge(
			protocol.SubjectNodeStructureChanged,
			protocol.StationBroadcast,
			&protocol.NodeStructureChanged{
				NodeID:   group.ID,
				NodeName: group.Name,
				Action:   "group_deleted",
			},
		)
	}

	h.jsonSuccess(w)
}
