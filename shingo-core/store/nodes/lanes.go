package nodes

import (
	"database/sql"
	"fmt"
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

// FindStoreSlotInLane finds the deepest empty slot in a lane for back-to-front packing.
// Returns *Node, but the WHERE clause checks the bins and orders tables — kept here
// because the return type is owned by nodes/.
func FindStoreSlotInLane(db *sql.DB, laneID int64) (*Node, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id = $1
		  AND n.is_synthetic = false
		  AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
		  AND NOT EXISTS (
			SELECT 1 FROM orders o
			WHERE o.delivery_node = n.name
			  AND o.status NOT IN ('confirmed', 'failed', 'cancelled')
		  )
		ORDER BY COALESCE(n.depth, 0) DESC
		LIMIT 1`, SelectCols, FromClause), laneID)
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
