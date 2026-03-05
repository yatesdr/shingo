package store

import (
	"fmt"
	"strconv"
)

// SupermarketSetup describes the structure for creating a supermarket node hierarchy.
type SupermarketSetup struct {
	Name         string
	Zone         string
	Lanes        []LaneSetup
	ShuffleSlots []string // vendor locations for shuffle slots
}

// LaneSetup describes a single lane in a supermarket.
type LaneSetup struct {
	Name            string
	Depth           int
	VendorLocations []string
}

// SupermarketCreateResult holds the IDs created by CreateSupermarket.
type SupermarketCreateResult struct {
	SupermarketID int64
	Name          string
}

// CreateSupermarket creates a full SMKT → LANE → Slot → SHUF hierarchy in a single transaction.
func (db *DB) CreateSupermarket(setup SupermarketSetup) (*SupermarketCreateResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	supType, err := db.GetNodeTypeByCode("SMKT")
	if err != nil {
		return nil, fmt.Errorf("SMKT node type not found")
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		return nil, fmt.Errorf("LANE node type not found")
	}
	shfType, err := db.GetNodeTypeByCode("SHUF")
	if err != nil {
		return nil, fmt.Errorf("SHUF node type not found")
	}

	// Create SMKT node
	supResult, err := tx.Exec(db.Q(`INSERT INTO nodes (name, node_type, node_type_id, zone, capacity, enabled) VALUES (?, 'storage', ?, ?, 0, 1)`),
		setup.Name, supType.ID, setup.Zone)
	if err != nil {
		return nil, fmt.Errorf("create supermarket node: %w", err)
	}
	supID, _ := supResult.LastInsertId()

	// Create lanes and slots
	for _, lane := range setup.Lanes {
		laneResult, err := tx.Exec(db.Q(`INSERT INTO nodes (name, node_type, node_type_id, parent_id, zone, capacity, enabled) VALUES (?, 'storage', ?, ?, ?, ?, 1)`),
			lane.Name, lanType.ID, supID, setup.Zone, lane.Depth)
		if err != nil {
			return nil, fmt.Errorf("create lane %s: %w", lane.Name, err)
		}
		laneID, _ := laneResult.LastInsertId()

		for d := 1; d <= lane.Depth; d++ {
			vendorLoc := ""
			if d-1 < len(lane.VendorLocations) {
				vendorLoc = lane.VendorLocations[d-1]
			}
			slotName := fmt.Sprintf("%s-S%02d", lane.Name, d)
			slotResult, err := tx.Exec(db.Q(`INSERT INTO nodes (name, vendor_location, node_type, parent_id, zone, capacity, enabled) VALUES (?, ?, 'storage', ?, ?, 1, 1)`),
				slotName, vendorLoc, laneID, setup.Zone)
			if err != nil {
				return nil, fmt.Errorf("create slot %s: %w", slotName, err)
			}
			slotID, _ := slotResult.LastInsertId()
			tx.Exec(db.Q(`INSERT INTO node_properties (node_id, key, value) VALUES (?, 'depth', ?)`), slotID, strconv.Itoa(d))
		}
	}

	// Create shuffle row
	shfResult, err := tx.Exec(db.Q(`INSERT INTO nodes (name, node_type, node_type_id, parent_id, zone, capacity, enabled) VALUES (?, 'storage', ?, ?, ?, 0, 1)`),
		setup.Name+"-SHUF", shfType.ID, supID, setup.Zone)
	if err != nil {
		return nil, fmt.Errorf("create shuffle row: %w", err)
	}
	shfID, _ := shfResult.LastInsertId()

	for i, loc := range setup.ShuffleSlots {
		slotName := fmt.Sprintf("%s-SHUF-%02d", setup.Name, i+1)
		slotResult, err := tx.Exec(db.Q(`INSERT INTO nodes (name, vendor_location, node_type, parent_id, zone, capacity, enabled) VALUES (?, ?, 'storage', ?, ?, 1, 1)`),
			slotName, loc, shfID, setup.Zone)
		if err != nil {
			return nil, fmt.Errorf("create shuffle slot %s: %w", slotName, err)
		}
		slotID, _ := slotResult.LastInsertId()
		tx.Exec(db.Q(`INSERT INTO node_properties (node_id, key, value) VALUES (?, 'role', 'shuffle')`), slotID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &SupermarketCreateResult{SupermarketID: supID, Name: setup.Name}, nil
}

// SupermarketSlotInfo describes a slot in a supermarket layout.
type SupermarketSlotInfo struct {
	NodeID   int64            `json:"node_id"`
	Name     string           `json:"name"`
	Depth    int              `json:"depth"`
	Instance *PayloadInstance `json:"instance,omitempty"`
}

// SupermarketLaneInfo describes a lane in a supermarket layout.
type SupermarketLaneInfo struct {
	Name  string                `json:"name"`
	ID    int64                 `json:"id"`
	Slots []SupermarketSlotInfo `json:"slots"`
}

// SupermarketLayout describes the full layout of a supermarket for visualization.
type SupermarketLayout struct {
	Lanes   []SupermarketLaneInfo `json:"lanes"`
	Shuffle []SupermarketSlotInfo `json:"shuffle"`
	Stats   SupermarketStats      `json:"stats"`
}

// SupermarketStats holds occupancy statistics for a supermarket.
type SupermarketStats struct {
	Total    int `json:"total"`
	Occupied int `json:"occupied"`
	Claimed  int `json:"claimed"`
}

// GetSupermarketLayout assembles the lane/slot/instance layout for a supermarket node.
func (db *DB) GetSupermarketLayout(supermarketID int64) (*SupermarketLayout, error) {
	children, err := db.ListChildNodes(supermarketID)
	if err != nil {
		return nil, err
	}

	layout := &SupermarketLayout{}

	for _, child := range children {
		if child.NodeTypeCode == "LANE" {
			slots, _ := db.ListLaneSlots(child.ID)
			var si []SupermarketSlotInfo
			for _, slot := range slots {
				depth, _ := db.GetSlotDepth(slot.ID)
				s := SupermarketSlotInfo{NodeID: slot.ID, Name: slot.Name, Depth: depth}
				instances, _ := db.ListInstancesByNode(slot.ID)
				if len(instances) > 0 {
					s.Instance = instances[0]
					layout.Stats.Occupied++
					if instances[0].ClaimedBy != nil {
						layout.Stats.Claimed++
					}
				}
				si = append(si, s)
				layout.Stats.Total++
			}
			layout.Lanes = append(layout.Lanes, SupermarketLaneInfo{Name: child.Name, ID: child.ID, Slots: si})
		} else if child.NodeTypeCode == "SHUF" {
			shfChildren, _ := db.ListChildNodes(child.ID)
			for _, slot := range shfChildren {
				s := SupermarketSlotInfo{NodeID: slot.ID, Name: slot.Name}
				instances, _ := db.ListInstancesByNode(slot.ID)
				if len(instances) > 0 {
					s.Instance = instances[0]
				}
				layout.Shuffle = append(layout.Shuffle, s)
			}
		}
	}

	return layout, nil
}

// DeleteSupermarket recursively deletes a supermarket and all its children in a transaction.
func (db *DB) DeleteSupermarket(supID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	children, _ := db.ListChildNodes(supID)
	for _, child := range children {
		grandchildren, _ := db.ListChildNodes(child.ID)
		for _, gc := range grandchildren {
			tx.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=?`), gc.ID)
			tx.Exec(db.Q(`DELETE FROM nodes WHERE id=?`), gc.ID)
		}
		tx.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=?`), child.ID)
		tx.Exec(db.Q(`DELETE FROM nodes WHERE id=?`), child.ID)
	}
	tx.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=?`), supID)
	tx.Exec(db.Q(`DELETE FROM nodes WHERE id=?`), supID)

	return tx.Commit()
}
