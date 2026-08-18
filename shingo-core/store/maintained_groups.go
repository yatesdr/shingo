package store

// Delegate file: maintained-group CRUD lives in store/nodes/maintain.go.
// Same shape as node_properties.go — the aggregate owns the queries, the outer
// store/ level owns the *DB method set the service layer calls.

import "shingocore/store/nodes"

// MaintainLevel and MaintainSupport are re-exported so callers outside the store
// do not have to import the nodes aggregate for the row types alone.
type (
	MaintainLevel   = nodes.MaintainLevel
	MaintainSupport = nodes.MaintainSupport
)

// SetMaintainLevel declares how many empty carriers of one type a maintained
// group holds.
func (db *DB) SetMaintainLevel(l MaintainLevel) error {
	return nodes.SetMaintainLevel(db.DB, l)
}

// RemoveMaintainLevel stops declaring a carrier type for a group.
func (db *DB) RemoveMaintainLevel(groupNodeID, binTypeID int64) error {
	return nodes.RemoveMaintainLevel(db.DB, groupNodeID, binTypeID)
}

// ListMaintainLevels returns a group's declared level, bin-type codes joined on.
func (db *DB) ListMaintainLevels(groupNodeID int64) ([]MaintainLevel, error) {
	return nodes.ListMaintainLevels(db.DB, groupNodeID)
}

// SetMaintainSupports replaces the set of process nodes a group serves.
func (db *DB) SetMaintainSupports(groupNodeID int64, processNodeIDs []int64) error {
	return nodes.SetMaintainSupports(db.DB, groupNodeID, processNodeIDs)
}

// ListMaintainSupports returns the process nodes a group serves, names joined on.
func (db *DB) ListMaintainSupports(groupNodeID int64) ([]MaintainSupport, error) {
	return nodes.ListMaintainSupports(db.DB, groupNodeID)
}
