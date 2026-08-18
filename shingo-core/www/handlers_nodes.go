package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"

	"shingocore/dispatch"
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

// apiSceneMarks is the picker's view of the scene: the labelled points a robot
// can be told to drive to, slimmed and searchable.
//
// WHY NOT /map/points, which already lists them. That endpoint answers the MAP's
// question — it returns every column including properties_json, the raw vendor
// property blob, because the map draws from it. A plant's scene carries a lot of
// location marks, and a picker that has to download every one of them with its
// property blob attached to show a dropdown is paying for a payload it discards.
// This returns the four fields a human picks by and nothing else.
//
// SEARCH IS SERVER-SIDE and matches the name, the label and the class, because a
// human looking for a waiting point knows it by whichever of those they were told.
// The cap is a guard, not paging: a picker showing 200 candidates has already
// failed at helping, and the honest response to "too many" is to say so and let
// them type more, not to silently truncate.
//
// AN EMPTY SCENE IS NOT AN ERROR. A sim backend with no scene sync, or a plant
// before its first sync, returns an empty list — and the picker falls back to
// typed entry, which is also the emergency path when the marks are stale.
func (h *Handlers) apiSceneMarks(w http.ResponseWriter, r *http.Request) {
	points, err := h.engine.NodeService().ListScenePoints()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type mark struct {
		Name  string `json:"name"`  // what the fleet is told; the value written to the property
		Label string `json:"label"` // the human name from the vendor scene, often blank
		Class string `json:"class"` // LocationMark, ParkPoint, … — the vendor's own class
		Area  string `json:"area"`
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	const cap = 200
	out := make([]mark, 0, 32)
	matched := 0
	for _, p := range points {
		if p == nil || p.InstanceName == "" {
			continue // a point with no name cannot be sent to the fleet
		}
		if q != "" && !strings.Contains(strings.ToLower(p.InstanceName), q) &&
			!strings.Contains(strings.ToLower(p.Label), q) &&
			!strings.Contains(strings.ToLower(p.ClassName), q) {
			continue
		}
		matched++
		if len(out) < cap {
			out = append(out, mark{
				Name:  p.InstanceName,
				Label: p.Label,
				Class: p.ClassName,
				Area:  p.AreaName,
			})
		}
	}

	h.jsonOK(w, map[string]any{
		"marks":     out,
		"matched":   matched,
		"truncated": matched > len(out),
	})
}

// apiLaneWaiting reports how many robots are dwelling at a lane's waiting point.
//
// It exists for one sentence in the UI: clearing a lane's mark while robots are
// waiting at it has to say how many, because that is the difference between an
// edit and an interruption. The count comes from the evaluator's own derivation
// (Dispatcher.GateStagedCount), so the number the human is shown is the number
// the machine is acting on.
func (h *Handlers) apiLaneWaiting(w http.ResponseWriter, r *http.Request) {
	laneID, err := strconv.ParseInt(r.URL.Query().Get("lane_id"), 10, 64)
	if err != nil || laneID == 0 {
		h.jsonError(w, "lane_id is required", http.StatusBadRequest)
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
	h.jsonOK(w, map[string]any{"lanes": out})
}

// apiSceneEdges returns the drivable path segments (advanced curves)
// synced from the fleet scene. Consumed by the robot-map dashboard to
// draw the travel network and route robots along real aisles instead of
// proximity-derived links.
func (h *Handlers) apiSceneEdges(w http.ResponseWriter, r *http.Request) {
	edges, err := h.engine.NodeService().ListSceneEdges()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, edges)
}

// ── The scene's own structure: areas and reflectors ────────────────────────
//
// Deliberately BESIDE /api/map/points and /api/map/edges rather than under a
// page-specific prefix. Structure is not owned by whichever page first needed
// it: the moment areas live under a confidence URL, the second consumer either
// re-implements them or imports a confidence endpoint to draw a wall.
//
// Both take an optional ?at=<RFC3339> and default to now. The parameter is the
// point — the scene is versioned, and a reader asking what the map looked like
// last Tuesday must not be silently answered with today's.

// atParam resolves the instant a scene query is asked about.
//
// A malformed timestamp is an ERROR, not a silent fallback to now. Quietly
// answering a different question than the one asked is how a reader ends up
// comparing this week's geometry against last week's numbers and concluding
// something moved.
func atParam(r *http.Request) (time.Time, error) {
	raw := r.URL.Query().Get("at")
	if raw == "" {
		return time.Now(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("at: %q is not an RFC3339 timestamp", raw)
	}
	return t, nil
}

// apiSceneAreas returns the declared map areas in force at an instant.
//
// The class is the field to render. Measured, the count of reflectors inside a
// zone has no predictive power over its no-estimate rate and the sign runs
// backwards; what predicts is whether it is a ReflectorArea. The count travels
// as provenance — "this declared reflector zone contains zero reflectors" is
// the most actionable sentence this work produced — and must not drive a mark.
func (h *Handlers) apiSceneAreas(w http.ResponseWriter, r *http.Request) {
	at, err := atParam(r)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	areas, err := h.engine.NodeService().SceneAreasAt(at)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, areas)
}

// apiSceneReflectors returns reflector positions in force at an instant.
func (h *Handlers) apiSceneReflectors(w http.ResponseWriter, r *http.Request) {
	at, err := atParam(r)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	reflectors, err := h.engine.NodeService().SceneReflectorsAt(at)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, reflectors)
}

// apiSceneDiffs returns the map change log, newest first, each row carrying
// the lanes it touched.
//
// This is what replaces a curated findings list on the diagnostic page: a
// standing narrative goes stale within a week, while "what changed, and to
// what" is the question an engineer actually arrives with.
func (h *Handlers) apiSceneDiffs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	diffs, err := h.engine.NodeService().RecentSceneDiffsWithLanes(limit)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, diffs)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

// apiRobotGroups lists the fleet's robot-dispatch groups for the payload-editor
// picker. Degrades gracefully on purpose: an RDS outage or a backend with no
// scene (e.g. the simulator) returns available=false + an empty list at 200, so
// the payload form falls back to free-text — the saved robot_group lives in
// Postgres and must never be lost just because the picker couldn't load.
func (h *Handlers) apiRobotGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.engine.RobotGroups()
	if err != nil {
		h.jsonOK(w, map[string]any{"available": false, "groups": []fleet.RobotGroup{}})
		return
	}
	h.jsonOK(w, map[string]any{"available": true, "groups": groups})
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
