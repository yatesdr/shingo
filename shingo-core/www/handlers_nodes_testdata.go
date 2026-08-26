package www

// The synthetic-node generator and its counterpart deleter.
//
// Split out of handlers_nodes.go (2026-08-19) — see handlers_scene.go for the
// reasoning. This is development tooling that happens to speak the node API: it
// bulk-creates a fake plant and then removes it again. Sitting beside the real
// node CRUD, it made both harder to read and made the file's length look like
// evidence that node handling was the complicated part.

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"shingocore/domain"
	"shingocore/engine"
)

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
				Name:    slotName,
				Zone:    "Production",
				Enabled: true,
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
