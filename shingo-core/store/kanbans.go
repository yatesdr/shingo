package store

// Delegate file: kanban CRUD lives in store/nodes/kanban.go.
// Same shape as node_properties.go — the aggregate owns the queries, the outer
// store/ level owns the *DB method set the service layer calls.

import (
	"fmt"
	"log"

	"shingocore/store/nodes"
)

// KanbanLevel and KanbanSupport are re-exported so callers outside the store
// do not have to import the nodes aggregate for the row types alone.
type (
	KanbanLevel     = nodes.KanbanLevel
	KanbanLayerWant = nodes.KanbanLayerWant
	KanbanSupport   = nodes.KanbanSupport
	TypeInPlay      = nodes.TypeInPlay
	Kanban          = nodes.Kanban
	KanbanSource    = nodes.KanbanSource
	OverflowTarget  = nodes.OverflowTarget
)

// SetKanbanLevel seeds one level with no audit: a fixture's write. Production
// saves go through SaveKanban.
func (db *DB) SetKanbanLevel(l KanbanLevel) error {
	return nodes.SetKanbanLevel(db.DB, l)
}

// SetKanbanLevelAndLayerWants seeds a level and its type's layer wants with no
// audit: a fixture's write. Production saves go through SaveKanban.
func (db *DB) SetKanbanLevelAndLayerWants(l KanbanLevel, wants []KanbanLayerWant) error {
	return nodes.SetKanbanLevelAndLayerWants(db.DB, l, wants)
}

// RemoveKanbanLevel removes a seeded level with no audit: a fixture's write.
// Production saves go through SaveKanban.
func (db *DB) RemoveKanbanLevel(groupNodeID, binTypeID int64) error {
	return nodes.RemoveKanbanLevel(db.DB, groupNodeID, binTypeID)
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

// ListKanbanLayerWants returns a group's layer wants, codes and each type's
// totes per layer joined on.
func (db *DB) ListKanbanLayerWants(groupNodeID int64) ([]KanbanLayerWant, error) {
	return nodes.ListKanbanLayerWants(db.DB, groupNodeID)
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

// ListKanbanLevels is the one read of a group's levels for everything that holds
// the plant to them: the resolver's cap, the capacity gate, the pass memo, the lift
// divert, the keeper's arithmetic. It answers through the picture, so Want is the
// effective want (kanban.Level.Want) and Base the declared number. Pins:
// TestKanbanPin_ALevelZeroRowLiftedByCover,
// TestKanbanArm_TheGapReadsTheEffectiveWant. The settings modal reads
// ListDeclaredLevels: a save writes a declared number and nothing else.
func (db *DB) ListKanbanLevels(groupNodeID int64) ([]KanbanLevel, error) {
	rows, err := db.ListKanbanPicture([]int64{groupNodeID})
	if err != nil {
		return nil, fmt.Errorf("list levels group=%d: %w", groupNodeID, err)
	}
	return LevelsOfPicture(rows), nil
}

// ListDeclaredLevels returns a group's declared level, bin-type codes joined on:
// the configuration's read, Want the number a person typed.
func (db *DB) ListDeclaredLevels(groupNodeID int64) ([]KanbanLevel, error) {
	return nodes.ListKanbanLevels(db.DB, groupNodeID)
}

// DeclaresLevel reports whether a group declares a level, in one read: the
// kanban-source test the loader save and the drain share
// (nodes.DeclaresLevel).
func (db *DB) DeclaresLevel(groupNodeID int64) (bool, error) {
	return nodes.DeclaresLevel(db.DB, groupNodeID)
}

// KanbanIDs is every group that declares a level, in one read.
func (db *DB) KanbanIDs() (map[int64]bool, error) {
	return nodes.KanbanIDs(db.DB)
}

// SetKanbanSupports seeds the set of process nodes a group serves with no
// audit: a fixture's write. Production saves go through SaveKanban.
func (db *DB) SetKanbanSupports(groupNodeID int64, processNodeIDs []int64) error {
	return nodes.SetKanbanSupports(db.DB, groupNodeID, processNodeIDs)
}

// ListKanbanSupports returns the process nodes a group serves, names joined on.
func (db *DB) ListKanbanSupports(groupNodeID int64) ([]KanbanSupport, error) {
	return nodes.ListKanbanSupports(db.DB, groupNodeID)
}

// GroupFencesAsker reports whether a strict kanban turns this asker
// away. See nodes.GroupFencesAsker — this is the disposition side of the fence,
// asked by a need that NAMED the group rather than by a plant-wide scan.
func (db *DB) GroupFencesAsker(groupNodeID int64, processNode string) (bool, error) {
	return nodes.GroupFencesAsker(db.DB, groupNodeID, processNode)
}

// KanbansSupporting returns the kanbans that serve a process
// node, by name. Empty for a process no group serves, which is most of them.
func (db *DB) KanbansSupporting(processNode string) ([]int64, error) {
	return nodes.KanbansSupporting(db.DB, processNode)
}

// KanbansSupportingProcess returns the kanbans a process's claims have a supported
// position in. One statement.
func (db *DB) KanbansSupportingProcess(processID string) ([]int64, error) {
	return nodes.KanbansSupportingProcess(db.DB, processID)
}

// NodeIsUnderAny reports whether a node sits inside any of the given subtrees.
func (db *DB) NodeIsUnderAny(nodeID int64, roots []int64) (bool, error) {
	return nodes.NodeIsUnderAny(db.DB, nodeID, roots)
}

// PerTypeOverflowTargetOf reads the overflow a store of one carrier type takes from
// a group (nodes.PerTypeOverflowTargetOf). One statement.
func (db *DB) PerTypeOverflowTargetOf(groupNodeID, binTypeID int64) (OverflowTarget, error) {
	return nodes.PerTypeOverflowTargetOf(db.DB, groupNodeID, binTypeID)
}
