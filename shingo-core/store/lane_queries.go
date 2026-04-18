package store

// Stage 2D delegate file: lane-scoped node queries live in store/nodes/.
// The bin-returning lane searches (FindSourceBinInLane, FindBuriedBin,
// FindOldestBuriedBin) stay here as cross-aggregate composition methods
// because their return type is *Bin (bins aggregate) while the WHERE
// clause joins nodes via parent_id.

import (
	"fmt"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// ListLaneSlots returns all child nodes of a lane, ordered by depth
// (ascending).
func (db *DB) ListLaneSlots(laneID int64) ([]*Node, error) {
	return nodes.ListLaneSlots(db.DB, laneID)
}

// GetSlotDepth returns the depth for a node, or 0 if not set.
func (db *DB) GetSlotDepth(nodeID int64) (int, error) {
	return nodes.GetSlotDepth(db.DB, nodeID)
}

// IsSlotAccessible returns true if no occupied slots exist at a shallower
// depth in the same lane.
func (db *DB) IsSlotAccessible(slotNodeID int64) (bool, error) {
	return nodes.IsSlotAccessible(db.DB, slotNodeID)
}

// FindSourceBinInLane finds the shallowest accessible unclaimed bin in a
// lane matching the given payload code. Cross-aggregate composition
// (bins ↔ nodes).
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
		LIMIT 1`, bins.BinJoinQuery)
	row := db.QueryRow(query, laneID, payloadCode)
	bin, err := bins.ScanBin(row)
	if err != nil {
		return nil, fmt.Errorf("no accessible bin in lane %d", laneID)
	}
	return bin, nil
}

// FindStoreSlotInLane finds the deepest empty slot in a lane for
// back-to-front packing.
func (db *DB) FindStoreSlotInLane(laneID int64) (*Node, error) {
	return nodes.FindStoreSlotInLane(db.DB, laneID)
}

// CountBinsInLane counts total bins across all slots in a lane.
func (db *DB) CountBinsInLane(laneID int64) (int, error) {
	return nodes.CountBinsInLane(db.DB, laneID)
}

// FindOldestBuriedBin finds the oldest buried bin in a lane by
// loaded_at/created_at timestamp. Unlike FindBuriedBin (which returns the
// shallowest buried bin for cheapest reshuffle), this returns the oldest
// buried bin for strict FIFO correctness. Cross-aggregate composition.
func (db *DB) FindOldestBuriedBin(laneID int64, payloadCode string) (*Bin, *Node, error) {
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
		ORDER BY COALESCE(b.loaded_at, b.created_at) ASC
		LIMIT 1`, bins.BinJoinQuery), laneID, payloadCode)
	bin, err := bins.ScanBin(row)
	if err != nil {
		return nil, nil, fmt.Errorf("no buried bin in lane %d", laneID)
	}
	slot, err := nodes.Get(db.DB, *bin.NodeID)
	if err != nil {
		return nil, nil, err
	}
	return bin, slot, nil
}

// FindBuriedBin finds a bin that exists in a lane but is blocked by
// shallower bins. Cross-aggregate composition (bins ↔ nodes).
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
		LIMIT 1`, bins.BinJoinQuery), laneID, payloadCode)
	bin, err := bins.ScanBin(row)
	if err != nil {
		return nil, nil, fmt.Errorf("no buried bin in lane %d", laneID)
	}
	slot, err := nodes.Get(db.DB, *bin.NodeID)
	if err != nil {
		return nil, nil, err
	}
	return bin, slot, nil
}
