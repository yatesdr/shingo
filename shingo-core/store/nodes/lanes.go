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
// Deepest-UNRESERVED (1d): the reservations NOT EXISTS makes a slot another order
// has soft-reserved (pending) or hard-claimed-and-confirmed invisible here, so two
// stores pack into distinct tiered slots. The bin-emptiness guard stays (a store
// wants a physically empty slot). The orders.delivery_node string-proxy STAYS too,
// NOT retired: the reservation read does NOT subsume it — simple store orders set
// delivery_node but do NOT reserve their slot in 1d (that's the #115/#117 gap
// deferred to unification, D26/D43), so the proxy is still the only guard against a
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
		ORDER BY COALESCE(n.depth, 0) DESC
		LIMIT 1`, SelectCols, FromClause, protocol.TerminalStatusSQLList()), laneID)
	n, err := ScanNode(row)
	if err != nil {
		return nil, fmt.Errorf("no empty slot in lane %d", laneID)
	}
	return n, nil
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
