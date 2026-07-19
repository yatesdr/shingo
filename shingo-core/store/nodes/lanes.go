package nodes

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
)

// ListLaneSlots returns all child nodes of a lane, ordered by depth (ascending).
func ListLaneSlots(db *sql.DB, laneID int64) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id=$1
		ORDER BY COALESCE(n.depth, 0) ASC`, SelectCols, FromClause), laneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanNodes(rows)
}

// GetSlotDepth returns the depth for a node, or 0 if not set.
func GetSlotDepth(db *sql.DB, nodeID int64) (int, error) {
	var depth *int
	err := db.QueryRow(`SELECT depth FROM nodes WHERE id=$1`, nodeID).Scan(&depth)
	if err != nil {
		return 0, err
	}
	if depth == nil {
		return 0, nil
	}
	return *depth, nil
}

// IsSlotAccessible returns true if no occupied slots exist at a shallower depth in the same lane.
func IsSlotAccessible(db *sql.DB, slotNodeID int64) (bool, error) {
	slot, err := Get(db, slotNodeID)
	if err != nil {
		return false, err
	}
	if slot.ParentID == nil {
		return true, nil
	}
	if slot.Depth == nil {
		return true, nil // no depth = accessible
	}

	var count int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM nodes sib
		JOIN bins b ON b.node_id = sib.id
		WHERE sib.parent_id = $1 AND sib.id != $2
		  AND sib.depth IS NOT NULL AND sib.depth < $3
	`, *slot.ParentID, slotNodeID, *slot.Depth).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// FindStoreSlotInLane finds the deepest empty, UNRESERVED slot in a lane for
// back-to-front packing. Returns *Node, but the WHERE clause checks the bins,
// reservations, and orders tables — kept here because the return type is owned by
// nodes/.
//
// Deepest-UNRESERVED: the reservations NOT EXISTS makes a slot another order
// has soft-reserved (pending) or hard-claimed-and-confirmed invisible here, so two
// stores pack into distinct tiered slots. The bin-emptiness guard stays (a store
// wants a physically empty slot). The orders.delivery_node string-proxy STAYS too,
// NOT retired: the reservation read does NOT subsume it — simple store orders set
// delivery_node but do NOT reserve their slot (that's the #115/#117 gap deferred to
// the dispatch-path unification), so the proxy is still the only guard against a
// complex store picking a slot a simple store is heading to. (Equivalence check
// result: gap found → proxy kept. Retire it when simple-store reserves its slot.)
func FindStoreSlotInLane(db *sql.DB, laneID int64) (*Node, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id = $1
		  AND n.is_synthetic = false
		  AND n.claimed_by IS NULL
		  AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
		  AND NOT EXISTS (
			SELECT 1 FROM reservations r
			WHERE r.node_id = n.id
			  AND r.resource_kind = 'slot'
			  AND r.state IN ('pending','confirmed')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM orders o
			WHERE o.delivery_node = n.name
			  AND o.status NOT IN (%s)
		  )
		  AND NOT EXISTS (
			-- Accessibility guard (mirrors IsSlotAccessible): a slot is only a
			-- valid pick if no OCCUPIED slot sits shallower in the same lane. The
			-- deepest-empty slot can otherwise be stranded behind a shallow bubble
			-- (an occupied slot with empties behind it), and a robot entering at
			-- the mouth could never reach it.
			SELECT 1 FROM nodes sib
			JOIN bins bb ON bb.node_id = sib.id
			WHERE sib.parent_id = n.parent_id
			  AND sib.depth IS NOT NULL
			  AND n.depth IS NOT NULL
			  AND sib.depth < n.depth
		  )
		ORDER BY COALESCE(n.depth, 0) DESC
		LIMIT 1`, SelectCols, FromClause, protocol.TerminalStatusSQLList()), laneID)
	n, err := ScanNode(row)
	if err != nil {
		return nil, fmt.Errorf("no empty slot in lane %d", laneID)
	}
	return n, nil
}

// LaneForNode returns the LANE node that directly parents nodeID, or (nil, nil)
// if nodeID is not a direct child slot of a lane. A one-hop parent walk (§8): a
// lane is modeled as a LANE-class node whose direct children are its
// depth-ordered slots, so a node's lane is its parent exactly when that parent is
// a LANE. Group-direct nodes, staging, and a lane node itself return (nil, nil) —
// they take no mouth exclusion. AuditLaneGeometry flags single-file geometry this
// walk cannot see.
func LaneForNode(db *sql.DB, nodeID int64) (*Node, error) {
	n, err := Get(db, nodeID)
	if err != nil {
		return nil, err
	}
	if n.ParentID == nil {
		return nil, nil
	}
	parent, err := Get(db, *n.ParentID)
	if err != nil {
		return nil, err
	}
	if parent.NodeTypeCode != protocol.NodeClassLANE {
		return nil, nil
	}
	return parent, nil
}

// AuditLaneGeometry returns human-readable warnings about single-file lane
// geometry that LaneForNode's one-hop parent walk cannot see, so a misconfigured
// scene is loud at startup rather than silently ungated (§8). Two smells:
//
//  1. A real (non-synthetic) node with a depth set whose parent is NOT a LANE —
//     it is tiered like a lane slot but hangs off a group directly, so it never
//     gets a mouth exclusion (the "NGRP-direct single-file" case).
//  2. A LANE nested directly under another LANE — the one-hop walk stops at the
//     inner lane and never reaches the outer, so the deeper slots stay ungated
//     (the "deep-nested" case).
//
// It is a diagnostic only: it changes nothing, and an empty result means the
// scene is fully walkable. Callers log each line at boot.
func AuditLaneGeometry(db *sql.DB) ([]string, error) {
	var warnings []string

	tiered, err := db.Query(`
		SELECT n.name, COALESCE(pnt.code, ''), pn.name
		FROM nodes n
		JOIN nodes pn ON pn.id = n.parent_id
		LEFT JOIN node_types pnt ON pnt.id = pn.node_type_id
		WHERE n.depth IS NOT NULL
		  AND n.is_synthetic = false
		  AND COALESCE(pnt.code, '') <> $1
		ORDER BY n.name`, protocol.NodeClassLANE)
	if err != nil {
		return nil, fmt.Errorf("audit lane geometry (tiered non-lane children): %w", err)
	}
	defer tiered.Close()
	for tiered.Next() {
		var name, parentClass, parentName string
		if err := tiered.Scan(&name, &parentClass, &parentName); err != nil {
			return nil, err
		}
		warnings = append(warnings, fmt.Sprintf(
			"node %q has a depth but its parent %q is %s, not a LANE — single-file geometry the mouth gate cannot see",
			name, parentName, classOrUntyped(parentClass)))
	}
	if err := tiered.Err(); err != nil {
		return nil, err
	}

	nested, err := db.Query(`
		SELECT n.name, pn.name
		FROM nodes n
		JOIN node_types nt ON nt.id = n.node_type_id
		JOIN nodes pn ON pn.id = n.parent_id
		JOIN node_types pnt ON pnt.id = pn.node_type_id
		WHERE nt.code = $1 AND pnt.code = $1
		ORDER BY n.name`, protocol.NodeClassLANE)
	if err != nil {
		return nil, fmt.Errorf("audit lane geometry (nested lanes): %w", err)
	}
	defer nested.Close()
	for nested.Next() {
		var name, parentName string
		if err := nested.Scan(&name, &parentName); err != nil {
			return nil, err
		}
		warnings = append(warnings, fmt.Sprintf(
			"lane %q is nested under lane %q — the one-hop walk cannot reach its slots",
			name, parentName))
	}
	return warnings, nested.Err()
}

func classOrUntyped(code string) string {
	if code == "" {
		return "untyped"
	}
	return code
}

// CountBinsInLane counts total bins across all slots in a lane.
// Lives here for convenience (single-table-coupled lane query) even though the
// COUNT runs over the bins table.
func CountBinsInLane(db *sql.DB, laneID int64) (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM bins b
		JOIN nodes slot ON slot.id = b.node_id
		WHERE slot.parent_id = $1
	`, laneID).Scan(&count)
	return count, err
}
