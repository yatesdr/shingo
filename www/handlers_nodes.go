package www

import (
	"net/http"
	"strconv"

	"warpath/engine"
	"warpath/store"
)

func (h *Handlers) handleNodes(w http.ResponseWriter, r *http.Request) {
	nodes, _ := h.engine.DB().ListNodes()
	states, _ := h.engine.NodeState().GetAllNodeStates()

	// Build count map and collect distinct zones
	counts := make(map[int64]int, len(nodes))
	zoneSet := map[string]bool{}
	for _, n := range nodes {
		if st, ok := states[n.ID]; ok {
			counts[n.ID] = st.ItemCount
		}
		if n.Zone != "" {
			zoneSet[n.Zone] = true
		}
	}
	zones := make([]string, 0, len(zoneSet))
	for z := range zoneSet {
		zones = append(zones, z)
	}

	data := map[string]any{
		"Page":          "nodes",
		"Nodes":         nodes,
		"Counts":        counts,
		"Zones":         zones,
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "nodes.html", data)
}

func (h *Handlers) handleNodeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	capacity, _ := strconv.Atoi(r.FormValue("capacity"))
	node := &store.Node{
		Name:        r.FormValue("name"),
		RDSLocation: r.FormValue("rds_location"),
		NodeType:    r.FormValue("node_type"),
		Zone:        r.FormValue("zone"),
		Capacity:    capacity,
		Enabled:     r.FormValue("enabled") == "on",
	}

	if err := h.engine.DB().CreateNode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.NodeState().RefreshNodeMeta(node.ID)
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
	node.RDSLocation = r.FormValue("rds_location")
	node.NodeType = r.FormValue("node_type")
	node.Zone = r.FormValue("zone")
	node.Capacity = capacity
	node.Enabled = r.FormValue("enabled") == "on"

	if err := h.engine.DB().UpdateNode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.NodeState().RefreshNodeMeta(node.ID)
	h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
		NodeID: node.ID, NodeName: node.Name, Action: "updated",
	}})

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
