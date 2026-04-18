package service

import (
	"errors"
	"fmt"

	"shingocore/store"
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
