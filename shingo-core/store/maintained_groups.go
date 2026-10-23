package store

// Delegate file: maintained-group CRUD lives in store/nodes/maintain.go.
// Same shape as node_properties.go — the aggregate owns the queries, the outer
// store/ level owns the *DB method set the service layer calls.

import "shingocore/store/nodes"

// MaintainLevel and MaintainSupport are re-exported so callers outside the store
// do not have to import the nodes aggregate for the row types alone.
type (
	MaintainLevel     = nodes.MaintainLevel
	MaintainLayerWant = nodes.MaintainLayerWant
	MaintainSupport   = nodes.MaintainSupport
	TypeInPlay        = nodes.TypeInPlay
)

// SetMaintainLevel declares how many empty carriers of one type a maintained
// group holds.
func (db *DB) SetMaintainLevel(l MaintainLevel) error {
	return nodes.SetMaintainLevel(db.DB, l)
}

// SetMaintainLevelAndLayerWants declares a level and replaces its type's layer
// wants, in one transaction.
func (db *DB) SetMaintainLevelAndLayerWants(l MaintainLevel, wants []MaintainLayerWant) error {
	return nodes.SetMaintainLevelAndLayerWants(db.DB, l, wants)
}

// RemoveMaintainLevel stops declaring a carrier type for a group, and removes
// its layer wants with it.
func (db *DB) RemoveMaintainLevel(groupNodeID, binTypeID int64) error {
	return nodes.RemoveMaintainLevel(db.DB, groupNodeID, binTypeID)
}

// ListMaintainLayerWants returns a group's layer wants, codes and each type's
// totes per layer joined on.
func (db *DB) ListMaintainLayerWants(groupNodeID int64) ([]MaintainLayerWant, error) {
	return nodes.ListMaintainLayerWants(db.DB, groupNodeID)
}

// ListTypesInPlay reads the bin types named by codes, with their dimensions and
// the group's layer wants of each, in one statement (nodes.ListTypesInPlay).
func (db *DB) ListTypesInPlay(groupNodeID int64, codes []string) ([]TypeInPlay, error) {
	return nodes.ListTypesInPlay(db.DB, groupNodeID, codes)
}

// TopsAndBottomsWant is the want of a level's tops & bottoms row, false when
// the group has none.
func (db *DB) TopsAndBottomsWant(groupNodeID, binTypeID int64) (int, bool, error) {
	return nodes.TopsAndBottomsWant(db.DB, groupNodeID, binTypeID)
}

// ListMaintainLevels returns a group's declared level, bin-type codes joined on.
func (db *DB) ListMaintainLevels(groupNodeID int64) ([]MaintainLevel, error) {
	return nodes.ListMaintainLevels(db.DB, groupNodeID)
}

// DeclaresLevel reports whether a group declares a level, in one read: the
// maintained-source test the loader save and the drain share
// (nodes.DeclaresLevel).
func (db *DB) DeclaresLevel(groupNodeID int64) (bool, error) {
	return nodes.DeclaresLevel(db.DB, groupNodeID)
}

// MaintainedGroupIDs is every group that declares a level, in one read.
func (db *DB) MaintainedGroupIDs() (map[int64]bool, error) {
	return nodes.MaintainedGroupIDs(db.DB)
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
