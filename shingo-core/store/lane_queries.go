package store

import "fmt"

// ListLaneSlots returns all child nodes of a lane, ordered by depth (ascending).
func (db *DB) ListLaneSlots(laneID int64) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id=$1
		ORDER BY COALESCE(n.depth, 0) ASC`, nodeSelectCols, nodeFromClause), laneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetSlotDepth returns the depth for a node, or 0 if not set.
func (db *DB) GetSlotDepth(nodeID int64) (int, error) {
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
func (db *DB) IsSlotAccessible(slotNodeID int64) (bool, error) {
	slot, err := db.GetNode(slotNodeID)
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

// FindSourceBinInLane finds the shallowest accessible unclaimed bin in a lane
// matching the given payload code. Uses a single query.
func (db *DB) FindSourceBinInLane(laneID int64, payloadCode string) (*Bin, error) {
	query := fmt.Sprintf(`%s
		WHERE b.node_id IN (SELECT id FROM nodes WHERE parent_id = $1)
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND b.status = 'available'
		  AND ($2 = '' OR b.payload_code = $2)
		  AND NOT EXISTS (
			SELECT 1 FROM nodes sib
			JOIN bins sb ON sb.node_id = sib.id
			WHERE sib.parent_id = $1
			  AND sib.depth IS NOT NULL
			  AND n.depth IS NOT NULL
			  AND sib.depth < n.depth
		  )
		ORDER BY COALESCE(n.depth, 0) ASC
		LIMIT 1`, binJoinQuery)
	row := db.QueryRow(query, laneID, payloadCode)
	bin, err := scanBin(row)
	if err != nil {
		return nil, fmt.Errorf("no accessible bin in lane %d", laneID)
	}
	return bin, nil
}

// FindStoreSlotInLane finds the deepest empty slot in a lane for back-to-front packing.
func (db *DB) FindStoreSlotInLane(laneID int64) (*Node, error) {
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
		LIMIT 1`, nodeSelectCols, nodeFromClause), laneID)
	n, err := scanNode(row)
	if err != nil {
		return nil, fmt.Errorf("no empty slot in lane %d", laneID)
	}
	return n, nil
}

// CountBinsInLane counts total bins across all slots in a lane.
func (db *DB) CountBinsInLane(laneID int64) (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM bins b
		JOIN nodes slot ON slot.id = b.node_id
		WHERE slot.parent_id = $1
	`, laneID).Scan(&count)
	return count, err
}

// FindBuriedBin finds a bin that exists in a lane but is blocked by shallower bins.
func (db *DB) FindBuriedBin(laneID int64, payloadCode string) (*Bin, *Node, error) {
	row := db.QueryRow(fmt.Sprintf(`%s
		WHERE b.node_id IN (SELECT id FROM nodes WHERE parent_id = $1)
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND b.status = 'available'
		  AND ($2 = '' OR b.payload_code = $2)
		  AND EXISTS (
			SELECT 1 FROM nodes sib
			JOIN bins sb ON sb.node_id = sib.id
			WHERE sib.parent_id = $1
			  AND sib.depth IS NOT NULL
			  AND n.depth IS NOT NULL
			  AND sib.depth < n.depth
		  )
		ORDER BY COALESCE(n.depth, 0) ASC
		LIMIT 1`, binJoinQuery), laneID, payloadCode)
	bin, err := scanBin(row)
	if err != nil {
		return nil, nil, fmt.Errorf("no buried bin in lane %d", laneID)
	}
	slot, err := db.GetNode(*bin.NodeID)
	if err != nil {
		return nil, nil, err
	}
	return bin, slot, nil
}
