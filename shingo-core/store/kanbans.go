package store

// Delegate file: kanban CRUD lives in store/nodes/kanban.go.
// Same shape as node_properties.go — the aggregate owns the queries, the outer
// store/ level owns the *DB method set the service layer calls.

import (
	"fmt"
	"log"

	"shingocore/store/bins"
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

// OverflowTargetOf reads a plain group's overflow property. One statement.
func (db *DB) OverflowTargetOf(groupNodeID int64) (OverflowTarget, error) {
	return nodes.OverflowTargetOf(db.DB, groupNodeID)
}

// OverflowNameOf is the name of a plain group's overflow, by the group's name; blank for none,
// and for a read that failed, which leaves the push parked until the next pass asks again. One
// statement.
func (db *DB) OverflowNameOf(groupName string) string {
	t, err := nodes.OverflowTargetByName(db.DB, groupName)
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

// ListKanbanPools reads a kanban's declared pools, by family id. One statement.
func (db *DB) ListKanbanPools(kanbanNodeID int64) (map[int64]int, error) {
	rows, err := db.DB.Query(`SELECT family_id, pool FROM kanban_families WHERE kanban_node_id = $1`, kanbanNodeID)
	if err != nil {
		return nil, fmt.Errorf("kanban pools %d: %w", kanbanNodeID, err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var family int64
		var pool int
		if err := rows.Scan(&family, &pool); err != nil {
			return nil, fmt.Errorf("kanban pools %d: scan: %w", kanbanNodeID, err)
		}
		out[family] = pool
	}
	return out, rows.Err()
}

// EffectiveTypesOfChildren is what each child of a group admits (bins.EffectiveTypesOfChildren).
func (db *DB) EffectiveTypesOfChildren(groupNodeID int64) (map[int64][]*bins.BinType, error) {
	return bins.EffectiveTypesOfChildren(db.DB, groupNodeID)
}

// KanbanFamilyClash is another kanban levelling a family on a support.
type KanbanFamilyClash struct{ Kanban, Family, Support string }

// KanbansLevellingAFamilyOn names the other kanbans that level one of these types' families on
// one of these supports. One statement, at a save.
func (db *DB) KanbansLevellingAFamilyOn(kanbanNodeID int64, types, supports []int64) ([]KanbanFamilyClash, error) {
	rows, err := db.DB.Query(`SELECT DISTINCT g.name, f.code, pn.name
		  FROM kanban_supports s
		  JOIN kanban_levels l ON l.kanban_node_id = s.kanban_node_id
		  JOIN bin_types bt ON bt.id = l.bin_type_id
		  JOIN carrier_families f ON f.id = bt.family_id
		  JOIN nodes g ON g.id = s.kanban_node_id
		  JOIN nodes pn ON pn.id = s.process_node_id
		 WHERE s.kanban_node_id <> $1 AND s.process_node_id = ANY($3::bigint[])
		   AND bt.family_id IN (SELECT family_id FROM bin_types WHERE id = ANY($2::bigint[]))
		 ORDER BY g.name, f.code, pn.name`, kanbanNodeID, types, supports)
	if err != nil {
		return nil, fmt.Errorf("kanbans on the supports of %d: %w", kanbanNodeID, err)
	}
	defer rows.Close()
	var out []KanbanFamilyClash
	for rows.Next() {
		var c KanbanFamilyClash
		if err := rows.Scan(&c.Kanban, &c.Family, &c.Support); err != nil {
			return nil, fmt.Errorf("kanbans on the supports of %d: scan: %w", kanbanNodeID, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimedTypesNotLevelled names the bin types a style of the kanban's supports puts on the line
// that the kanban declares no level of. One statement, at a save.
func (db *DB) ClaimedTypesNotLevelled(kanbanNodeID int64) ([]string, error) {
	rows, err := db.DB.Query(`
		WITH scope AS (SELECT $1::bigint AS node_id),`+kanbanPictureCTEs+`
		SELECT DISTINCT bt.code
		  FROM style_part sp
		  JOIN bin_types bt ON bt.id = sp.bin_type_id
		  JOIN style_claims sc ON sc.process_id = sp.process_id AND sc.style_id = sp.style_id AND sc.payload_code = sp.payload_code
		  JOIN positions pos ON pos.node_name IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node)
		 WHERE NOT EXISTS (SELECT 1 FROM kanban_levels l WHERE l.kanban_node_id = $1 AND l.bin_type_id = bt.id)
		 ORDER BY bt.code`, kanbanNodeID)
	if err != nil {
		return nil, fmt.Errorf("claimed types of %d: %w", kanbanNodeID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("claimed types of %d: scan: %w", kanbanNodeID, err)
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// ReferencesToNodeName says what joins a node by its name: loaders' outbound and inbound, and
// claims' positions, each in words a person can look up. One statement.
func (db *DB) ReferencesToNodeName(name string) ([]string, error) {
	rows, err := db.DB.Query(`
		SELECT 'loader ' || l.name || ' (outbound)' FROM bin_loaders l WHERE l.archived_at IS NULL AND l.outbound_dest = $1
		UNION ALL
		SELECT 'loader ' || l.name || ' (inbound)' FROM bin_loaders l WHERE l.archived_at IS NULL AND l.inbound_source = $1
		UNION ALL
		SELECT DISTINCT 'claim of ' || sc.process_id || ' / ' || sc.style_id FROM style_claims sc
		 WHERE $1 IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node, sc.inbound_source, sc.outbound_destination)
		 ORDER BY 1`, name)
	if err != nil {
		return nil, fmt.Errorf("references to %s: %w", name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("references to %s: scan: %w", name, err)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}
