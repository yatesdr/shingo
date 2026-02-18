package www

import (
	"log"
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

func (h *Handlers) handleNodeSyncRDS(w http.ResponseWriter, r *http.Request) {
	bins, err := h.engine.RDSClient().GetBinDetails()
	if err != nil {
		log.Printf("node sync: RDS error: %v", err)
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}

	created := 0
	for _, bin := range bins {
		// Check if node already exists by RDS location or name
		if _, err := h.engine.DB().GetNodeByRDSLocation(bin.ID); err == nil {
			continue
		}
		if _, err := h.engine.DB().GetNodeByName(bin.ID); err == nil {
			continue
		}
		node := &store.Node{
			Name:        bin.ID,
			RDSLocation: bin.ID,
			NodeType:    "storage",
			Capacity:    1,
			Enabled:     true,
		}
		if err := h.engine.DB().CreateNode(node); err != nil {
			continue
		}
		h.engine.NodeState().RefreshNodeMeta(node.ID)
		h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
			NodeID: node.ID, NodeName: node.Name, Action: "created",
		}})
		created++
	}

	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (h *Handlers) handleSceneSync(w http.ResponseWriter, r *http.Request) {
	scene, err := h.engine.RDSClient().GetScene()
	if err != nil {
		log.Printf("scene sync: RDS error: %v", err)
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}

	// Build point-to-area map
	pointArea := make(map[string]string)
	for _, area := range scene.Areas {
		for _, pt := range area.Points {
			pointArea[pt] = area.Name
		}
	}

	nodes, _ := h.engine.DB().ListNodes()
	for _, node := range nodes {
		if node.RDSLocation == "" || node.Zone != "" {
			continue
		}
		if zone, ok := pointArea[node.RDSLocation]; ok {
			node.Zone = zone
			h.engine.DB().UpdateNode(node)
			h.engine.Events.Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
				NodeID: node.ID, NodeName: node.Name, Action: "updated",
			}})
		}
	}

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

func (h *Handlers) apiBinOccupancy(w http.ResponseWriter, r *http.Request) {
	bins, err := h.engine.RDSClient().GetBinDetails()
	if err != nil {
		h.jsonError(w, "RDS error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	nodes, _ := h.engine.DB().ListNodes()

	// Build lookup maps
	binMap := make(map[string]bool, len(bins))
	for _, b := range bins {
		binMap[b.ID] = b.Filled
	}

	nodeRDS := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.RDSLocation != "" {
			nodeRDS[n.RDSLocation] = n.Name
		}
	}

	type entry struct {
		BinID       string `json:"bin_id"`
		NodeName    string `json:"node_name"`
		RDSFilled   *bool  `json:"rds_filled"`
		InWarPath   bool   `json:"in_warpath"`
		Discrepancy string `json:"discrepancy"`
	}

	var results []entry

	// Bins in RDS
	for _, b := range bins {
		e := entry{
			BinID:     b.ID,
			RDSFilled: &b.Filled,
			InWarPath: nodeRDS[b.ID] != "",
			NodeName:  nodeRDS[b.ID],
		}
		if !e.InWarPath {
			e.Discrepancy = "rds_only"
		}
		results = append(results, e)
	}

	// Nodes in WarPath but not in RDS
	for _, n := range nodes {
		if n.RDSLocation == "" {
			continue
		}
		if _, ok := binMap[n.RDSLocation]; !ok {
			results = append(results, entry{
				BinID:       n.RDSLocation,
				NodeName:    n.Name,
				InWarPath:   true,
				Discrepancy: "warpath_only",
			})
		}
	}

	h.jsonOK(w, results)
}
