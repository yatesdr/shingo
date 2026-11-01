package store

// Delegate file: maintained-group CRUD lives in store/nodes/maintain.go.
// Same shape as node_properties.go — the aggregate owns the queries, the outer
// store/ level owns the *DB method set the service layer calls.

import (
	"fmt"
	"log"

	"shingocore/store/nodes"
)

// MaintainLevel and MaintainSupport are re-exported so callers outside the store
// do not have to import the nodes aggregate for the row types alone.
type (
	MaintainLevel     = nodes.MaintainLevel
	MaintainLayerWant = nodes.MaintainLayerWant
	MaintainSupport   = nodes.MaintainSupport
	TypeInPlay        = nodes.TypeInPlay
	Kanban            = nodes.Kanban
	KanbanSource      = nodes.KanbanSource
	OverflowTarget    = nodes.OverflowTarget
)

// SetMaintainLevel seeds one level with no audit: a fixture's write. Production
// saves go through SaveKanban.
func (db *DB) SetMaintainLevel(l MaintainLevel) error {
	return nodes.SetMaintainLevel(db.DB, l)
}

// SetMaintainLevelAndLayerWants seeds a level and its type's layer wants with no
// audit: a fixture's write. Production saves go through SaveKanban.
func (db *DB) SetMaintainLevelAndLayerWants(l MaintainLevel, wants []MaintainLayerWant) error {
	return nodes.SetMaintainLevelAndLayerWants(db.DB, l, wants)
}

// RemoveMaintainLevel removes a seeded level with no audit: a fixture's write.
// Production saves go through SaveKanban.
func (db *DB) RemoveMaintainLevel(groupNodeID, binTypeID int64) error {
	return nodes.RemoveMaintainLevel(db.DB, groupNodeID, binTypeID)
}

// GetKanban reads one kanbans row, nil when the group has none.
func (db *DB) GetKanban(nodeID int64) (*Kanban, error) {
	return nodes.GetKanban(db.DB, nodeID)
}

// ListKanbans reads every kanbans row in one statement.
func (db *DB) ListKanbans() ([]Kanban, error) {
	return nodes.ListKanbans(db.DB)
}

// ListKanbanSources returns a kanban's sources in rank order, names joined on.
func (db *DB) ListKanbanSources(kanbanNodeID int64) ([]KanbanSource, error) {
	return nodes.ListKanbanSources(db.DB, kanbanNodeID)
}

// OverflowTargetOf reads a group's overflow: a kanban's row, or a plain group's
// property. One statement.
func (db *DB) OverflowTargetOf(groupNodeID int64) (OverflowTarget, error) {
	return nodes.OverflowTargetOf(db.DB, groupNodeID)
}

// OverflowNameOf is the name of the overflow a store of one carrier type takes from
// a group, by the group's name (0: the type is not known, the group's own); blank
// for none, and for a read that failed, which leaves the push parked until the
// next pass asks again. One statement.
func (db *DB) OverflowNameOf(groupName string, binTypeID int64) string {
	t, err := nodes.PerTypeOverflowTargetByName(db.DB, groupName, binTypeID)
	if err != nil {
		log.Printf("store: %v", err)
		return ""
	}
	return t.Name
}

// ListMaintainLayerWants returns a group's layer wants, codes and each type's
// totes per layer joined on.
func (db *DB) ListMaintainLayerWants(groupNodeID int64) ([]MaintainLayerWant, error) {
	return nodes.ListMaintainLayerWants(db.DB, groupNodeID)
}

// ListTypesInPlay reads the bin types named by codes, with their dimensions and
// the groups' layer wants of each, in one statement (nodes.ListTypesInPlay).
func (db *DB) ListTypesInPlay(groupNodeIDs []int64, codes []string) ([]TypeInPlay, error) {
	return nodes.ListTypesInPlay(db.DB, groupNodeIDs, codes)
}

// TopsAndBottomsWant is the want of a level's tops & bottoms row, false when
// the group has none.
func (db *DB) TopsAndBottomsWant(groupNodeID, binTypeID int64) (int, bool, error) {
	return nodes.TopsAndBottomsWant(db.DB, groupNodeID, binTypeID)
}

// ListMaintainLevels is the one read of a group's levels for everything that holds
// the plant to them: the resolver's cap, the capacity gate, the pass memo, the lift
// divert, the keeper's arithmetic. It answers through the picture, so Want is the
// effective want (kanban.Level.Want) and Base the declared number. Pins:
// TestKanbanPin_ALevelZeroRowLiftedByCover,
// TestMaintainerArm_TheGapReadsTheEffectiveWant. The settings modal reads
// ListDeclaredLevels: a save writes a declared number and nothing else.
func (db *DB) ListMaintainLevels(groupNodeID int64) ([]MaintainLevel, error) {
	rows, err := db.ListKanbanPicture([]int64{groupNodeID})
	if err != nil {
		return nil, fmt.Errorf("list levels group=%d: %w", groupNodeID, err)
	}
	return LevelsOfPicture(rows), nil
}

// ListDeclaredLevels returns a group's declared level, bin-type codes joined on:
// the configuration's read, Want the number a person typed.
func (db *DB) ListDeclaredLevels(groupNodeID int64) ([]MaintainLevel, error) {
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

// SetMaintainSupports seeds the set of process nodes a group serves with no
// audit: a fixture's write. Production saves go through SaveKanban.
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

// KanbansSupportingProcess returns the kanbans a process's claims have a supported
// position in. One statement.
func (db *DB) KanbansSupportingProcess(processID string) ([]int64, error) {
	return nodes.KanbansSupportingProcess(db.DB, processID)
}

// NodeIsUnderAny reports whether a node sits inside any of the given subtrees.
// The MG3-5 audit's second read: did this carrier come from within a group that
// serves the press that took it.
func (db *DB) NodeIsUnderAny(nodeID int64, roots []int64) (bool, error) {
	return nodes.NodeIsUnderAny(db.DB, nodeID, roots)
}

// PerTypeOverflowTargetOf reads the overflow a store of one carrier type takes from
// a group (nodes.PerTypeOverflowTargetOf). One statement.
func (db *DB) PerTypeOverflowTargetOf(groupNodeID, binTypeID int64) (OverflowTarget, error) {
	return nodes.PerTypeOverflowTargetOf(db.DB, groupNodeID, binTypeID)
}
