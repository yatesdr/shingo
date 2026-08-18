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

// GroupFencesAsker reports whether a strict maintained group turns this asker
// away. See nodes.GroupFencesAsker — this is the disposition side of the fence,
// asked by a need that NAMED the group rather than by a plant-wide scan.
func (db *DB) GroupFencesAsker(groupNodeID int64, processNode string) (bool, error) {
	return nodes.GroupFencesAsker(db.DB, groupNodeID, processNode)
}

// MaintainedGroupsSupporting returns the maintained groups that serve a process
// node, by name. Empty for a process no group serves, which is most of them.
func (db *DB) MaintainedGroupsSupporting(processNode string) ([]int64, error) {
	return nodes.MaintainedGroupsSupporting(db.DB, processNode)
}

// NodeIsUnderAny reports whether a node sits inside any of the given subtrees.
// The MG3-5 audit's second read: did this carrier come from within a group that
// serves the press that took it.
func (db *DB) NodeIsUnderAny(nodeID int64, roots []int64) (bool, error) {
	return nodes.NodeIsUnderAny(db.DB, nodeID, roots)
}
