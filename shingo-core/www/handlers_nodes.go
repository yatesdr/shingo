package www

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"shingo/protocol"

	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/engine"
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

// apiLaneWaiting reports how many robots are dwelling at a lane's waiting point.
//
// It exists for one sentence in the UI: clearing a lane's mark while robots are
// waiting at it has to say how many, because that is the difference between an
// edit and an interruption. The count comes from the evaluator's own derivation
// (Dispatcher.GateStagedCount), so the number the human is shown is the number
// the machine is acting on.
func (h *Handlers) apiLaneWaiting(w http.ResponseWriter, r *http.Request) {
	// GROUP FIRST, because the waiting spots are the group's and clearing them
	// stands down every lane in the block. A confirmation scoped to one lane
	// would quote a number far smaller than the interruption it is about to
	// cause, which is worse than not asking: the person reads it and proceeds.
	if groupID, err := strconv.ParseInt(r.URL.Query().Get("group_id"), 10, 64); err == nil && groupID != 0 {
		n, cErr := h.engine.Dispatcher().GroupGateStagedCount(groupID)
		if cErr != nil {
			h.jsonError(w, cErr.Error(), http.StatusInternalServerError)
			return
		}
		h.jsonOK(w, map[string]any{"waiting": n})
		return
	}
	laneID, err := strconv.ParseInt(r.URL.Query().Get("lane_id"), 10, 64)
	if err != nil || laneID == 0 {
		h.jsonError(w, "lane_id or group_id is required", http.StatusBadRequest)
		return
	}
	n, err := h.engine.Dispatcher().GateStagedCount(laneID)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{"waiting": n})
}

// apiLaneGatePoints lists a group's lanes with the waiting point each one has.
//
// One call for the whole section. The alternative — the picker fetching each
// lane's detail to read one property — is fifteen requests on modal open at the
// seeded plant and more at a real one, for a value that is one column.
func (h *Handlers) apiLaneGatePoints(w http.ResponseWriter, r *http.Request) {
	groupID, err := strconv.ParseInt(r.URL.Query().Get("group_id"), 10, 64)
	if err != nil || groupID == 0 {
		h.jsonError(w, "group_id is required", http.StatusBadRequest)
		return
	}
	children, err := h.engine.NodeService().ListChildNodes(groupID)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type laneGate struct {
		LaneID int64  `json:"lane_id"`
		Name   string `json:"name"`
		Point  string `json:"point"`
	}
	out := make([]laneGate, 0, len(children))
	for _, c := range children {
		if c == nil || c.NodeTypeCode != protocol.NodeClassLANE {
			continue
		}
		out = append(out, laneGate{
			LaneID: c.ID,
			Name:   c.Name,
			Point:  h.engine.NodeService().GetNodeProperty(c.ID, dispatch.PropLaneGatePoint),
		})
	}
	// THE GROUP'S OWN LIST, in the same call, because it is the primary control
	// now and the per-lane rows are the legacy override beside it. Two calls
	// would let the section render the overrides before the thing they override.
	h.jsonOK(w, map[string]any{
		"lanes":             out,
		"group_wait_points": h.engine.NodeService().GetNodeProperty(groupID, dispatch.PropGroupWaitPoints),
	})
}
func (h *Handlers) handleNodes(w http.ResponseWriter, r *http.Request) {
	pd, err := getNodesPageData(&nodesPageDataAdapter{ns: h.engine.NodeService(), bs: h.engine.BinService()})
	if err != nil {
		log.Printf("nodes page: get page data: %v", err)
	}

	binTypesJSON, _ := json.Marshal(pd.BinTypes)
	edgesJSON, _ := json.Marshal(pd.Edges)

	data := map[string]any{
		"Page":          "nodes",
		"Nodes":         pd.Nodes,
		"Counts":        pd.Counts,
		"TileStates":    pd.TileStates,
		"Zones":         pd.Zones,
		"NodeLabels":    pd.NodeLabels,
		"NodeInfo":      pd.NodeInfo,
		"MapGroups":     pd.MapGroups,
		"MapClassOrder": []string{"ActionPoint", "ChargePoint", "LocationMark"},
		"BinTypes":      pd.BinTypes,
		"Edges":         pd.Edges,
		"ChildCounts":   pd.ChildCounts,
		"Depths":        pd.Depths,
		"BinTypesJSON":  string(binTypesJSON),
		"EdgesJSON":     string(edgesJSON),
	}
	h.render(w, r, "nodes.html", data)
}
func (h *Handlers) handleNodeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rawName := r.FormValue("name")
	name := strings.TrimSpace(rawName)
	if name != rawName {
		log.Printf("WARNING admin handleNodeCreate: trimmed whitespace from name %q", rawName)
	}

	node := &domain.Node{
		Name:    name,
		Zone:    r.FormValue("zone"),
		Enabled: r.FormValue("enabled") == "on",
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

	rawName := r.FormValue("name")
	name := strings.TrimSpace(rawName)
	if name != rawName {
		log.Printf("WARNING admin handleNodeUpdate: trimmed whitespace from name %q", rawName)
	}
	node.Name = name
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

	// NodeService.ApplyAssignments always writes the station + bin-type mode,
	// even when the form posts an empty mode — that matches the pre-refactor
	// update-path behavior where an empty mode was written through verbatim.
	a := parseNodeAssignments(r)

	// The holds-bins guard, BEFORE anything is written. Narrowing what a
	// MAINTAINED group may hold can strand the carriers already standing in it;
	// for every other node, and for a widening, this is a no-op.
	//
	// The browser asks the question first (via /maintained-group/check-types) and
	// carries the answer here as force, because this form POST navigates and a
	// 409 would replace the page with an error document. The guard still runs on
	// this side so a caller that skipped the question is still refused.
	if a.BinTypeMode == "specific" {
		g, gErr := h.engine.NodeService().CheckMaintainedGroupTypesChange(
			node.ID, a.BinTypeIDs, r.FormValue("force") == "on")
		if gErr != nil {
			http.Error(w, gErr.Error(), http.StatusInternalServerError)
			return
		}
		if g.Blocked != "" {
			http.Error(w, g.Blocked, http.StatusConflict)
			return
		}
	}

	// THE OLD ASSIGNMENTS FIRST, for the audit rows below.
	beforeAssign := h.nodeAssignmentSnapshot(node.ID)

	if err := h.engine.NodeService().UpdateNode(node); err != nil {
		// THIS is the path a parentage cycle can actually arrive by. The
		// drag-and-drop reparent endpoint already refuses these shapes for
		// unrelated reasons (its parent must be a lane or group, and a synthetic
		// node cannot be reparented at all), but this form posts parent_id
		// straight through with no such restriction. 400, not 500: the form is
		// well-formed and the operator fixes it by choosing a different parent,
		// and the message already names the chain that makes it impossible.
		code := http.StatusInternalServerError
		if service.IsParentCycle(err) {
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}

	if err := h.engine.NodeService().ApplyAssignments(node.ID, a); err != nil {
		log.Printf("node update: apply assignments for node %d: %v", node.ID, err)
	}

	// AUDITED — and this is a bigger hole than the one phase 1 set out to close.
	// apiSetNodeBinTypes was named as "the one unaudited write in the modal", but
	// the modal's Allowed Bins and Allowed Stations controls do not go through
	// it: they ride this form POST into ApplyAssignments, which writes four
	// things and audited none of them. Every property write beside them on the
	// same screen has left a trail since the waiting point landed.
	//
	// It stopped being tidiness when press typing started reading node_bin_types:
	// what a position will accept becomes the thing that types an empty pull, so
	// "who narrowed this and when" is a question somebody will ask about an
	// incident.
	h.auditNodeAssignments(node.ID, beforeAssign)

	h.engine.EventBus().Emit(engine.Event{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{
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

// apiNodeOccupancy is POTENTIAL DEAD CODE (2026-06-12). Node occupancy reads
// live bin positions from RDS/SEER (GetNodeOccupancy → GET /binDetails), but RDS
// bin tracking was never set up in production, so this errors on real plants and
// the "Check Occupancy" button has been hidden (see templates/nodes.html). The
// route + handler are kept (not deleted) in case RDS bin tracking ships; remove
// them if it stays unimplemented.
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
		"node":                node,
		"stations":            stations,
		"bin_types":           binTypes,
		"properties":          props,
		"children":            children,
		"effective_stations":  effectiveStations,
		"effective_bin_types": effectiveBinTypes,
		"bin_type_mode":       binTypeMode,
		"station_mode":        stationMode,
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
	// A WAIT POINT MAY NOT BE SHARED BETWEEN GROUPS, and this is one of the two
	// doors where a person is typing one and can be told. The READ path refuses
	// nothing on purpose: a config check on the dispatch hot path strands every
	// robot already standing at that point rather than the person who typed it.
	// The other door is the seeder; the startup census reports what got through.
	if req.Key == dispatch.PropGroupWaitPoints {
		conflicts, err := h.engine.Dispatcher().DuplicateWaitPoints(
			req.NodeID, dispatch.ParseWaitPoints(req.Value))
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(conflicts) > 0 {
			// BOTH GROUPS NAMED. "duplicate wait point" tells the person nothing
			// they can act on; the second name is the whole of what they need.
			h.jsonError(w, strings.Join(conflicts, "; ")+" — a wait point is a physical place beside "+
				"ONE block of lanes, and a robot sent to another block's spot is standing in "+
				"somebody's aisle", http.StatusBadRequest)
			return
		}
	}
	if err := h.setNodePropertyAudited(req.NodeID, req.Key, req.Value); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// setNodePropertyAudited writes one node property and leaves the old→new trail
// behind it.
//
// EXTRACTED SO THERE IS ONE OF IT. The maintained-group settings endpoint writes
// four properties in one save, and the alternative — a second place that calls
// SetNodeProperty and remembers to append an audit row — is how the trail
// acquires a hole nobody notices until they go looking for a change that was
// never recorded. Anything that writes a node property from the UI goes through
// here.
//
// AUDITED, and this is where the waiting point earns it. Node properties are
// configuration — lane_gate_point decides whether a lane stages robots at all —
// and configuration that changes without a trail is the thing nobody can
// reconstruct afterwards. Same shape as every other admin write (bin_actions).
// Unconditional rather than keyed to the interesting keys: a list of which
// properties deserve an audit row is a list that goes stale.
func (h *Handlers) setNodePropertyAudited(nodeID int64, key, value string) error {
	// THE OLD VALUE FIRST, because an audit row that cannot say what changed
	// records that somebody touched something. Best-effort: a read that fails must
	// not stop the write, it only makes the trail poorer.
	before := h.engine.NodeService().GetNodeProperty(nodeID, key)

	if err := h.engine.NodeService().SetNodeProperty(nodeID, key, value); err != nil {
		return err
	}

	if before != value {
		if err := h.engine.AuditService().Append("node", nodeID, "property:"+key,
			before, value, protocol.AuditActorUI); err != nil {
			log.Printf("node property set: audit %s on node %d: %v", key, nodeID, err)
		}
	}
	return nil
}

// apiSetNodeBinTypes replaces bin type assignments for a node.
func (h *Handlers) apiSetNodeBinTypes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID     int64   `json:"node_id"`
		BinTypeIDs []int64 `json:"bin_type_ids"`
		// Force answers the holds-bins guard below.
		Force bool `json:"force"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.NodeID == 0 {
		h.jsonError(w, "node_id is required", http.StatusBadRequest)
		return
	}
	// THE OLD SET FIRST, for the same reason the property write reads the old
	// value first: an audit row that cannot say what changed records only that
	// somebody touched something. Best-effort — a read that fails must not stop
	// the write, it only makes the trail poorer.
	before := h.nodeBinTypeCodes(req.NodeID)

	// Narrowing what a MAINTAINED group may hold can strand the carriers already
	// standing in it. The guard is a no-op for every other node, and for a
	// widening: it only fires when a resident's type is on its way out.
	g, err := h.engine.NodeService().CheckMaintainedGroupTypesChange(req.NodeID, req.BinTypeIDs, req.Force)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.maintainedGroupHeld(w, g) {
		return
	}

	if err := h.engine.NodeService().SetNodeBinTypes(req.NodeID, req.BinTypeIDs); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// AUDITED — this was the one write in the group settings modal that left no
	// trail, while every property write beside it left one. It stopped being a
	// tidiness item when press typing started reading this table: what a position
	// will accept becomes the thing that types an empty pull, so "who narrowed
	// this and when" is a question somebody will ask about an incident.
	//
	// CODES, not ids. The row is read by a person reconstructing a change, and a
	// list of integers requires a second lookup against a table that may have
	// moved on since.
	after := h.nodeBinTypeCodes(req.NodeID)
	if before != after {
		if err := h.engine.AuditService().Append("node", req.NodeID, "bin_types",
			before, after, protocol.AuditActorUI); err != nil {
			log.Printf("set node bin types: audit on node %d: %v", req.NodeID, err)
		}
	}
	h.jsonSuccess(w)
}

// nodeBinTypeCodes renders a node's directly-assigned carrier types as a stable,
// comma-joined code list for the audit trail. Empty string means none assigned,
// which on this table means "no restriction" — a distinction the audit row
// carries by saying nothing, exactly as the resolver reads it.
func (h *Handlers) nodeBinTypeCodes(nodeID int64) string {
	bts, err := h.engine.NodeService().ListBinTypesForNode(nodeID)
	if err != nil {
		log.Printf("set node bin types: read current types on node %d: %v", nodeID, err)
		return ""
	}
	codes := make([]string, 0, len(bts))
	for _, bt := range bts {
		codes = append(codes, bt.Code)
	}
	// ListTypesForNode already orders by code, so the two sides of the audit row
	// are comparable without sorting here.
	return strings.Join(codes, ", ")
}

// nodeAssignmentSnapshot is the four values ApplyAssignments writes, as the
// audit trail renders them.
//
// CODES AND NAMES, not ids: the row is read by a person reconstructing a change,
// and a list of integers needs a second lookup against a table that may have
// moved on since. Best-effort throughout — a read that fails must not stop the
// write, it only makes the trail poorer, which is the same posture the property
// endpoint takes.
type nodeAssignmentSnapshot struct {
	stationMode string
	stations    string
	binTypeMode string
	binTypes    string
}

func (h *Handlers) nodeAssignmentSnapshot(nodeID int64) nodeAssignmentSnapshot {
	svc := h.engine.NodeService()
	snap := nodeAssignmentSnapshot{
		stationMode: svc.GetNodeProperty(nodeID, "station_mode"),
		binTypeMode: svc.GetNodeProperty(nodeID, "bin_type_mode"),
		binTypes:    h.nodeBinTypeCodes(nodeID),
	}
	if sts, err := svc.ListStationsForNode(nodeID); err == nil {
		sorted := append([]string(nil), sts...)
		sort.Strings(sorted)
		snap.stations = strings.Join(sorted, ", ")
	} else {
		log.Printf("node update: read current stations on node %d: %v", nodeID, err)
	}
	return snap
}

// auditNodeAssignments appends one row per assignment that actually moved.
//
// PER FIELD, not one combined row: an operator asking "when did this stop
// accepting the big carrier" is asking about bin_types, and a row that bundles
// four values makes them read three they did not ask about to find out.
func (h *Handlers) auditNodeAssignments(nodeID int64, before nodeAssignmentSnapshot) {
	after := h.nodeAssignmentSnapshot(nodeID)
	rows := []struct{ action, old, new string }{
		{"station_mode", before.stationMode, after.stationMode},
		{"stations", before.stations, after.stations},
		{"bin_type_mode", before.binTypeMode, after.binTypeMode},
		{"bin_types", before.binTypes, after.binTypes},
	}
	for _, row := range rows {
		if row.old == row.new {
			continue
		}
		if err := h.engine.AuditService().Append("node", nodeID, row.action,
			row.old, row.new, protocol.AuditActorUI); err != nil {
			log.Printf("node update: audit %s on node %d: %v", row.action, nodeID, err)
		}
	}
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
