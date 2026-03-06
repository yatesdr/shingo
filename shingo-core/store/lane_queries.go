package store

import (
	"fmt"
	"strconv"
)

// ListLaneSlots returns all child nodes of a lane, ordered by depth (ascending).
func (db *DB) ListLaneSlots(laneID int64) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id=?
		ORDER BY CAST(COALESCE(
			(SELECT np.value FROM node_properties np WHERE np.node_id=n.id AND np.key='depth'), '0'
		) AS INTEGER) ASC`, nodeSelectCols, nodeFromClause)), laneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetSlotDepth reads the "depth" property for a node.
func (db *DB) GetSlotDepth(nodeID int64) (int, error) {
	var val string
	err := db.QueryRow(db.Q(`SELECT value FROM node_properties WHERE node_id=? AND key='depth'`), nodeID).Scan(&val)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(val)
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

	depth, err := db.GetSlotDepth(slotNodeID)
	if err != nil {
		return true, nil // no depth property = accessible
	}

	// Check if any shallower slot (depth < this depth) has a bin
	var count int
	err = db.QueryRow(db.Q(`
		SELECT COUNT(*) FROM nodes sib
		JOIN node_properties dp ON dp.node_id=sib.id AND dp.key='depth'
		JOIN bins b ON b.node_id=sib.id
		WHERE sib.parent_id=? AND sib.id!=? AND CAST(dp.value AS INTEGER) < ?
	`), *slot.ParentID, slotNodeID, depth).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// FindSourcePayloadInLane finds the FIFO-oldest accessible unclaimed payload in a lane.
func (db *DB) FindSourcePayloadInLane(laneID int64, blueprintCode string) (*Payload, error) {
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		return nil, err
	}

	// Walk from front (shallowest) to back, find first accessible slot with matching payload
	for _, slot := range slots {
		payloads, err := db.ListPayloadsByNode(slot.ID)
		if err != nil || len(payloads) == 0 {
			continue
		}

		for _, p := range payloads {
			if p.ClaimedBy != nil || p.Status != "available" || p.BinStatus == "staged" {
				continue
			}
			if blueprintCode != "" && p.BlueprintCode != blueprintCode {
				continue
			}
			// This slot is accessible (front-most occupied slot)
			return p, nil
		}
		// If this slot is occupied but doesn't match, deeper slots are blocked
		if len(payloads) > 0 {
			break
		}
	}
	return nil, fmt.Errorf("no accessible payload in lane %d", laneID)
}

// FindStoreSlotInLane finds the deepest empty slot in a lane for back-to-front packing.
func (db *DB) FindStoreSlotInLane(laneID int64, blueprintID int64) (*Node, error) {
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		return nil, err
	}

	// Walk from back (deepest) to front, find first empty slot
	for i := len(slots) - 1; i >= 0; i-- {
		slot := slots[i]
		count, err := db.CountBinsByNode(slot.ID)
		if err != nil {
			continue
		}
		inflight, _ := db.CountActiveOrdersByDeliveryNode(slot.Name)
		if count+inflight == 0 {
			return slot, nil
		}
	}
	return nil, fmt.Errorf("no empty slot in lane %d", laneID)
}

// CountBinsInLane counts total bins across all slots in a lane.
func (db *DB) CountBinsInLane(laneID int64) (int, error) {
	var count int
	err := db.QueryRow(db.Q(`
		SELECT COUNT(*) FROM bins b
		JOIN nodes slot ON slot.id = b.node_id
		WHERE slot.parent_id = ?
	`), laneID).Scan(&count)
	return count, err
}

// FindBuriedPayload finds a payload that exists in a lane but is blocked by shallower bins.
func (db *DB) FindBuriedPayload(laneID int64, blueprintCode string) (*Payload, *Node, error) {
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		return nil, nil, err
	}

	for _, slot := range slots {
		payloads, err := db.ListPayloadsByNode(slot.ID)
		if err != nil || len(payloads) == 0 {
			continue
		}
		for _, p := range payloads {
			if p.ClaimedBy != nil || p.Status != "available" || p.BinStatus == "staged" {
				continue
			}
			if blueprintCode != "" && p.BlueprintCode != blueprintCode {
				continue
			}
			// Check if it's actually buried (not accessible)
			accessible, _ := db.IsSlotAccessible(slot.ID)
			if !accessible {
				return p, slot, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("no buried payload in lane %d", laneID)
}
