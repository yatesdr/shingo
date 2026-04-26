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

// ListScenePointsByClass returns the scene points whose class name
// matches the filter. Absorbed from engine_db_methods.go as part of
// the Phase 3a closeout (PR 3a.6).
func (s *NodeService) ListScenePointsByClass(className string) ([]*scene.Point, error) {
	return s.db.ListScenePointsByClass(className)
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
