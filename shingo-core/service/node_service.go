package service

import (
	"errors"
	"fmt"

	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/inventory"
	"shingocore/store/nodes"
	"shingocore/store/registry"
	"shingocore/store/scene"
	"shingocore/store/sceneversion"
	"time"
)

// NodeService centralizes the node-assignment composite flow that used
// to live inline in www/handlers_nodes.go's create/update handlers.
//
// The node row itself is still created/updated through the engine
// accessors (CreateNode / UpdateNode) so that the existing audit and
// event boundaries stay exactly where they are. What this service owns
// is the 4-step "apply station mode + stations + bin-type mode + bin
// types" flow which was the bulk of the handler LOC and is shared
// verbatim between the create and update paths.
type NodeService struct {
	db *store.DB

	// names caches uid→display_name for station label rendering. One row per
	// plant, dropped whole on rename/enroll. See station_names.go.
	names stationNameCache
}

func NewNodeService(db *store.DB) *NodeService {
	return &NodeService{db: db}
}

// NodeAssignments bundles the station + bin-type selections that are
// persisted alongside a node as properties and assignment rows.
//
//   - StationMode / BinTypeMode: "specific", "inherit", "all", or empty.
//     The mode is always written (empty value clears the property).
//     Any non-"specific" mode clears the assignment list.
//   - Stations / BinTypeIDs: only honored when the corresponding mode
//     is "specific". Safe to leave nil otherwise — the service clears
//     the assignment list for you.
type NodeAssignments struct {
	StationMode string
	Stations    []string
	BinTypeMode string
	BinTypeIDs  []int64
}

// CreateNodeGroup creates a new node group (synthetic NGRP node) with
// the given name and returns its ID. Absorbed from
// engine_db_methods.go as part of the www-handler service migration.
func (s *NodeService) CreateNodeGroup(name string) (int64, error) {
	return s.db.CreateNodeGroup(name)
}

// AddLane appends a new lane (synthetic LANE node) under the given
// node group and returns the new lane's ID. Absorbed from
// engine_db_methods.go as part of the www-handler service migration.
func (s *NodeService) AddLane(groupID int64, name string) (int64, error) {
	return s.db.AddLane(groupID, name)
}

// DeleteNodeGroup removes a node group along with its lane and slot
// children. Absorbed from engine_db_methods.go as part of the
// www-handler service migration.
func (s *NodeService) DeleteNodeGroup(groupID int64) error {
	return s.db.DeleteNodeGroup(groupID)
}

// GetGroupLayout returns the layout (lanes + slots + counts) for the
// given node group. Absorbed from engine_db_methods.go as part of the
// www-handler service migration.
func (s *NodeService) GetGroupLayout(groupID int64) (*store.GroupLayout, error) {
	return s.db.GetGroupLayout(groupID)
}

// ListLaneSlots returns the ordered slot children of a lane. Absorbed
// from engine_db_methods.go as part of the www-handler service
// migration.
func (s *NodeService) ListLaneSlots(laneID int64) ([]*nodes.Node, error) {
	return s.db.ListLaneSlots(laneID)
}

// ReorderLaneSlots rewrites the depth ordering of a lane's slot
// children to match orderedNodeIDs. Absorbed from engine_db_methods.go
// as part of the www-handler service migration.
func (s *NodeService) ReorderLaneSlots(laneID int64, orderedNodeIDs []int64) error {
	return s.db.ReorderLaneSlots(laneID, orderedNodeIDs)
}

// SetNodePayloads replaces the payload assignment list for the given
// node. Pass nil or empty to clear. Absorbed from engine_db_methods.go
// as part of the www-handler service migration.
func (s *NodeService) SetNodePayloads(nodeID int64, payloadIDs []int64) error {
	return s.db.SetNodePayloads(nodeID, payloadIDs)
}

// SetNodeStations replaces the station assignment list for the given
// node. Pass nil or empty to clear. Absorbed from engine_db_methods.go
// as part of the www-handler service migration.
func (s *NodeService) SetNodeStations(nodeID int64, stationIDs []string) error {
	return s.db.SetNodeStations(nodeID, stationIDs)
}

// CreateNode inserts a new node row and populates its ID. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.1b).
func (s *NodeService) CreateNode(n *nodes.Node) error {
	return s.db.CreateNode(n)
}

// UpdateNode persists changes to a node row. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.1b).
func (s *NodeService) UpdateNode(n *nodes.Node) error {
	return s.db.UpdateNode(n)
}

// DeleteNode removes a node row. Absorbed from engine_db_methods.go
// as part of the www-handler service migration (PR 3a.1b).
func (s *NodeService) DeleteNode(id int64) error {
	return s.db.DeleteNode(id)
}

// GetNode loads a node by ID. Absorbed from engine_db_methods.go as
// part of the www-handler service migration (PR 3a.1b).
func (s *NodeService) GetNode(id int64) (*nodes.Node, error) {
	return s.db.GetNode(id)
}

// ListNodes returns every node in the store. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.1b).
func (s *NodeService) ListNodes() ([]*nodes.Node, error) {
	return s.db.ListNodes()
}

// ListChildNodes returns the direct children of a parent node.
// Absorbed from engine_db_methods.go as part of the www-handler
// service migration (PR 3a.1b).
func (s *NodeService) ListChildNodes(parentID int64) ([]*nodes.Node, error) {
	return s.db.ListChildNodes(parentID)
}

// NodeTileStates returns the aggregated tile-state snapshot keyed by
// node ID (bin counts, occupancy indicators, etc.). Absorbed from
// engine_db_methods.go as part of the nodesPageDataStore dissolution
// (PR 3a.5.1).
func (s *NodeService) NodeTileStates() (map[int64]bins.NodeTileState, error) {
	return s.db.NodeTileStates()
}

// ListScenePoints returns every scene point registered in the store.
// Absorbed from engine_db_methods.go as part of the nodesPageDataStore
// dissolution (PR 3a.5.1). Scene data is node-adjacent: points map to
// node locations via their instance names.
func (s *NodeService) ListScenePoints() ([]*scene.Point, error) {
	return s.db.ListScenePoints()
}

// ListEdges returns the registered edges (adjacency records) between
// nodes. Absorbed from engine_db_methods.go as part of the
// nodesPageDataStore dissolution (PR 3a.5.1).
func (s *NodeService) ListEdges() ([]registry.Edge, error) {
	return s.db.ListEdges()
}

// GetSlotDepth returns the depth of a slot within its containing lane.
// Absorbed from engine_db_methods.go as part of the nodesPageDataStore
// dissolution (PR 3a.5.1).
func (s *NodeService) GetSlotDepth(nodeID int64) (int, error) {
	return s.db.GetSlotDepth(nodeID)
}

// ── PR 3a.6 additions: remaining www-reachable queries ───────────────────

// GetByName loads a node by its human-readable name (instance name).
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6).
func (s *NodeService) GetByName(name string) (*nodes.Node, error) {
	return s.db.GetNodeByName(name)
}

// GetByDotName resolves a dotted hierarchical name (e.g. "GROUP.LANE.SLOT")
// into the corresponding leaf node. Absorbed from engine_db_methods.go
// as part of the Phase 3a closeout (PR 3a.6). Internal engine and
// dispatch flows still call *store.DB.GetNodeByDotName directly — this
// method is the handler-layer entry point only.
func (s *NodeService) GetByDotName(name string) (*nodes.Node, error) {
	return s.db.GetNodeByDotName(name)
}

// ListScenePointsByArea returns the scene points registered under a
// given area name. Absorbed from engine_db_methods.go as part of the
// Phase 3a closeout (PR 3a.6).
func (s *NodeService) ListScenePointsByArea(areaName string) ([]*scene.Point, error) {
	return s.db.ListScenePointsByArea(areaName)
}

// StationsForPointName resolves a point name a robot reports
// (`rbk_report.current_station`, e.g. "AP102") to the station(s) the
// scene binds to it. See scene.StationsForPointName for why the answer
// is a slice and why more than one row is a refusal, not a choice.
func (s *NodeService) StationsForPointName(pointName string) ([]string, error) {
	return s.db.StationsForPointName(pointName)
}

// ClassOfPoint reports what the scene calls a point — ChargePoint,
// ParkPoint, LocationMark, ActionPoint — so a refusal can say what the
// robot was standing at instead of only that it was not a station.
func (s *NodeService) ClassOfPoint(instanceName string) (string, error) {
	return s.db.ClassOfPoint(instanceName)
}

// CountStationPoints reports how many bin locations the scene holds at
// all. Asked only on the refusal path, to tell "not a station" from
// "Core has never synced a scene".
func (s *NodeService) CountStationPoints() (int, error) {
	return s.db.CountStationPoints()
}

// ListScenePointsByClass returns the scene points whose class name
// matches the filter. Absorbed from engine_db_methods.go as part of
// the Phase 3a closeout (PR 3a.6).
func (s *NodeService) ListScenePointsByClass(className string) ([]*scene.Point, error) {
	return s.db.ListScenePointsByClass(className)
}

// ListSceneEdges returns the drivable path segments synced from the
// fleet scene (advanced curves). The robot-map dashboard renders these
// as its travel network and routes robots along them.
func (s *NodeService) ListSceneEdges() ([]*scene.Edge, error) {
	return s.db.ListSceneEdges()
}

// ── The scene as it was, not merely as it is ───────────────────────────────
//
// These take an INSTANT rather than defaulting to now. A query written against
// "the current map" returns different attribution for the same historical
// sample before and after an edit, which is the failure scene versioning
// exists to prevent — so a caller that wants now says so.

// SceneAreasAt returns the declared areas in force at an instant.
//
// These come from the ROBOT's own .smap, not from RDS: RDS's /scene returns
// advancedAreaList: [] and has never exposed them. Nothing else in the system
// can answer "where are the reflector zones".
func (s *NodeService) SceneAreasAt(at time.Time) ([]sceneversion.AreaView, error) {
	return s.db.SceneAreasAt(at)
}

// SceneReflectorsAt returns the reflector positions in force at an instant.
func (s *NodeService) SceneReflectorsAt(at time.Time) ([]sceneversion.ReflectorView, error) {
	return s.db.SceneReflectorsAt(at)
}

// RecentSceneDiffs returns the map change log, newest first.
func (s *NodeService) RecentSceneDiffs(limit int) ([]sceneversion.DiffView, error) {
	return s.db.RecentSceneDiffs(limit)
}

// SceneDiff is one observed map edit WITH the lanes it touched.
//
// Composed here rather than in the handler because www may not reach into a
// store sub-package for a type — handlers talk to services, which is what
// keeps the HTTP layer from growing a second opinion about the schema.
type SceneDiff struct {
	sceneversion.DiffView
	// Lanes is the part of a diff row that does real work. An engineer
	// arrives asking "what did I touch"; "17 objects changed" does not
	// answer it.
	Lanes []string `json:"lanes"`
}

// RecentSceneDiffsWithLanes returns the map change log, newest first, each row
// carrying the lanes that edit touched.
func (s *NodeService) RecentSceneDiffsWithLanes(limit int) ([]SceneDiff, error) {
	diffs, err := s.db.RecentSceneDiffs(limit)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(diffs))
	for _, d := range diffs {
		ids = append(ids, d.ID)
	}
	// One query for the page, not one per row. At the limit of 50 this read used
	// to cost 51 round trips, and the localization board's compare mode fetches
	// two boards, so 102.
	lanesByDiff, err := s.db.LanesChangedByDiffs(ids)
	if err != nil {
		return nil, err
	}
	out := make([]SceneDiff, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, SceneDiff{DiffView: d, Lanes: lanesByDiff[d.ID]})
	}
	return out, nil
}

// ListCorrectionsByNode returns the most recent correction entries
// filed against a single node, capped at limit rows. Absorbed from
// engine_db_methods.go as part of the Phase 3a closeout (PR 3a.6).
func (s *NodeService) ListCorrectionsByNode(nodeID int64, limit int) ([]*inventory.Correction, error) {
	return s.db.ListCorrectionsByNode(nodeID, limit)
}

// ListNodeStates returns the per-node state snapshot keyed by node ID.
// Absorbed from engine_db_methods.go as part of the www-handler
// service migration (PR 3a.1b).
func (s *NodeService) ListNodeStates() (map[int64]*store.NodeState, error) {
	return s.db.ListNodeStates()
}

// ListBinsByNode returns the bins currently at a node. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.1b).
func (s *NodeService) ListBinsByNode(nodeID int64) ([]*bins.Bin, error) {
	return s.db.ListBinsByNode(nodeID)
}

// ListBinsByNodes is ListBinsByNode over a set of nodes in one query. Same
// filter and same ORDER BY, so grouping the result by node id gives each node
// exactly what the per-node call would have returned.
func (s *NodeService) ListBinsByNodes(nodeIDs []int64) ([]*bins.Bin, error) {
	return s.db.ListBinsByNodes(nodeIDs)
}

// ListStationsForNode returns the explicit station assignments for a
// node. Absorbed from engine_db_methods.go as part of the www-handler
// service migration (PR 3a.1b).
func (s *NodeService) ListStationsForNode(nodeID int64) ([]string, error) {
	return s.db.ListStationsForNode(nodeID)
}

// ListBinTypesForNode returns the explicit bin-type assignments for a
// node. Absorbed from engine_db_methods.go as part of the www-handler
// service migration (PR 3a.1b).
func (s *NodeService) ListBinTypesForNode(nodeID int64) ([]*bins.BinType, error) {
	return s.db.ListBinTypesForNode(nodeID)
}

// ListNodeProperties returns all properties associated with a node.
// Absorbed from engine_db_methods.go as part of the www-handler
// service migration (PR 3a.1b).
func (s *NodeService) ListNodeProperties(nodeID int64) ([]*nodes.Property, error) {
	return s.db.ListNodeProperties(nodeID)
}

// GetEffectiveStations resolves the station list visible at a node,
// taking inheritance modes into account. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.1b).
func (s *NodeService) GetEffectiveStations(nodeID int64) ([]string, error) {
	return s.db.GetEffectiveStations(nodeID)
}

// GetEffectiveBinTypes resolves the bin-type list visible at a node,
// taking inheritance modes into account. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.1b).
func (s *NodeService) GetEffectiveBinTypes(nodeID int64) ([]*bins.BinType, error) {
	return s.db.GetEffectiveBinTypes(nodeID)
}

// GetNodeProperty returns the value of a single node property, or the
// empty string if the property is not set. Absorbed from
// engine_db_methods.go as part of the www-handler service migration
// (PR 3a.1b).
func (s *NodeService) GetNodeProperty(nodeID int64, key string) string {
	return s.db.GetNodeProperty(nodeID, key)
}

// SetNodeProperty upserts a key/value property on a node. Absorbed
// from engine_db_methods.go as part of the www-handler service
// migration (PR 3a.1b).
func (s *NodeService) SetNodeProperty(nodeID int64, key, value string) error {
	return s.db.SetNodeProperty(nodeID, key, value)
}

// DeleteNodeProperty removes a key/value property from a node.
// Absorbed from engine_db_methods.go as part of the www-handler
// service migration (PR 3a.1b).
func (s *NodeService) DeleteNodeProperty(nodeID int64, key string) error {
	return s.db.DeleteNodeProperty(nodeID, key)
}

// SetNodeBinTypes replaces the explicit bin-type assignments for a
// node. Pass nil or empty to clear. Absorbed from engine_db_methods.go
// as part of the www-handler service migration (PR 3a.1b).
func (s *NodeService) SetNodeBinTypes(nodeID int64, binTypeIDs []int64) error {
	return s.db.SetNodeBinTypes(nodeID, binTypeIDs)
}

// ReparentNode moves a node under a new parent at the given position.
// Absorbed from engine_db_methods.go as part of the www-handler
// service migration (PR 3a.1b).
func (s *NodeService) ReparentNode(nodeID int64, parentID *int64, position int) error {
	return s.db.ReparentNode(nodeID, parentID, position)
}

// ApplyAssignments writes the station and bin-type selections for a
// node. Each sub-step is best-effort: if one step fails the others
// still run, and all errors are joined and returned so the caller can
// log a single combined message. This mirrors the pre-refactor handler
// behavior, where assignment failures were logged but did not abort
// the node create/update flow.
func (s *NodeService) ApplyAssignments(nodeID int64, a NodeAssignments) error {
	var errs []error

	// Station mode + station assignments.
	if err := s.db.SetNodeProperty(nodeID, "station_mode", a.StationMode); err != nil {
		errs = append(errs, fmt.Errorf("set station_mode: %w", err))
	}
	if a.StationMode == "specific" {
		if err := s.db.SetNodeStations(nodeID, a.Stations); err != nil {
			errs = append(errs, fmt.Errorf("set stations: %w", err))
		}
	} else {
		if err := s.db.SetNodeStations(nodeID, nil); err != nil {
			errs = append(errs, fmt.Errorf("clear stations: %w", err))
		}
	}

	// Bin-type mode + bin-type assignments.
	if err := s.db.SetNodeProperty(nodeID, "bin_type_mode", a.BinTypeMode); err != nil {
		errs = append(errs, fmt.Errorf("set bin_type_mode: %w", err))
	}
	if a.BinTypeMode == "specific" {
		if err := s.db.SetNodeBinTypes(nodeID, a.BinTypeIDs); err != nil {
			errs = append(errs, fmt.Errorf("set bin types: %w", err))
		}
	} else {
		if err := s.db.SetNodeBinTypes(nodeID, nil); err != nil {
			errs = append(errs, fmt.Errorf("clear bin types: %w", err))
		}
	}

	return errors.Join(errs...)
}

// ── Edge station enrollment (identity, v66) ──────────────────────────────
//
// ENROLLMENT IS AN ADMIN ACT WITH A HUMAN IN IT, and that is the design rather
// than an unfinished automation. Core cannot distinguish "a new station has
// arrived" from "this station's hardware was replaced" by looking at a Pi —
// the two look identical from the network, which is exactly why the old code
// could not tell them apart and silently picked one interpretation. The human
// who is physically holding the box is the only party that knows, so the
// answer is expressed by which of these two things they do:
//
//	NEW STATION       → EnrollEdge, take the fresh uid, put it on the Pi.
//	REPLACED HARDWARE → do NOT enroll. Read the existing uid off Core
//	                    (GetEdgeByUID / the edges list), put THAT on the new
//	                    Pi, and RebindEdgeHostname to move the lease.
//
// The second is the case a first-boot UUID cannot express at all.

// ErrAlreadyEnrolled / ErrUnknownStation re-export the registry sentinels so
// handlers can classify an enrollment outcome without importing store/registry
// — the same shape as ErrInventoryDeltaSkipped, and required by the
// www-no-direct-store depguard rule.
var (
	ErrAlreadyEnrolled = registry.ErrAlreadyEnrolled
	ErrUnknownStation  = registry.ErrUnknownStation
)

// EnrollEdge mints a station identity. displayName is the operator's label and
// is not an identifier; pass "" to default it to the uid.
func (s *NodeService) EnrollEdge(displayName string) (*registry.Edge, error) {
	uid, err := registry.NewStationUID()
	if err != nil {
		return nil, err
	}
	e, err := s.db.EnrollEdge(uid, displayName, uid)
	if err != nil {
		return nil, err
	}
	s.invalidateStationNames()
	return e, nil
}

// GetEdge returns one enrolled station by uid.
func (s *NodeService) GetEdge(uid string) (*registry.Edge, error) { return s.db.GetEdgeByUID(uid) }

// RenameEdge sets the operator-facing display name. Free of consequence by
// construction — see registry.SetDisplayName.
func (s *NodeService) RenameEdge(uid, displayName string) (bool, error) {
	ok, err := s.db.RenameEdge(uid, displayName)
	// Invalidate whenever the write reached the database, matched or not: a
	// no-match wrote nothing, but the cheapest correct rule is "the write path
	// ran, drop the cache" and a spurious reload of a one-row map costs nothing.
	if err == nil {
		s.invalidateStationNames()
	}
	return ok, err
}

// ClaimEdge records a human's answer to "what is this station?" — the act an
// edge cannot perform for itself. Naming is optional everywhere else; here it
// doubles as the acknowledgement, because the two happen at the same moment.
func (s *NodeService) ClaimEdge(uid, displayName string) (bool, error) {
	ok, err := s.db.ClaimEdge(uid, displayName)
	if err == nil {
		s.invalidateStationNames()
	}
	return ok, err
}

// RebindEdge moves a station's binding to a new machine and clears its
// conflict record. The sanctioned "yes, this station lives here now".
func (s *NodeService) RebindEdge(uid, hostname string) (bool, error) {
	return s.db.RebindEdgeHostname(uid, hostname)
}
