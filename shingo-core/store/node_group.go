package store

import (
	"fmt"
	"strings"
)

// CreateNodeGroup creates an empty NGRP node with the given name.
// Lanes and direct children are added separately via AddLane and drag-and-drop reparenting.
func (db *DB) CreateNodeGroup(name string) (int64, error) {
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		return 0, fmt.Errorf("NGRP node type not found")
	}
	id, err := db.insertID(`INSERT INTO nodes (name, is_synthetic, node_type_id, enabled) VALUES ($1, true, $2, true) RETURNING id`,
		name, grpType.ID)
	if err != nil {
		return 0, fmt.Errorf("create node group: %w", err)
	}
	return id, nil
}

// AddLane creates a LANE node as a child of the given node group.
func (db *DB) AddLane(groupID int64, name string) (int64, error) {
	grpNode, err := db.GetNode(groupID)
	if err != nil {
		return 0, fmt.Errorf("node group not found: %w", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		return 0, fmt.Errorf("LANE node type not found")
	}
	laneID, err := db.insertID(`INSERT INTO nodes (name, is_synthetic, node_type_id, parent_id, zone, enabled) VALUES ($1, true, $2, $3, $4, true) RETURNING id`,
		name, lanType.ID, groupID, grpNode.Zone)
	if err != nil {
		return 0, fmt.Errorf("create lane: %w", err)
	}
	return laneID, nil
}

// GroupSlotInfo describes a slot in a node group layout.
type GroupSlotInfo struct {
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	Depth  int    `json:"depth"`
	Bin    *Bin   `json:"bin,omitempty"`
}

// GroupLaneInfo describes a lane in a node group layout.
type GroupLaneInfo struct {
	Name  string          `json:"name"`
	ID    int64           `json:"id"`
	Slots []GroupSlotInfo `json:"slots"`
}

// GroupLayout describes the full layout of a node group for visualization.
type GroupLayout struct {
	Lanes       []GroupLaneInfo `json:"lanes"`
	DirectNodes []GroupSlotInfo `json:"direct_nodes"`
	Stats       GroupStats      `json:"stats"`
}

// GroupStats holds occupancy statistics for a node group.
type GroupStats struct {
	Total    int `json:"total"`
	Occupied int `json:"occupied"`
	Claimed  int `json:"claimed"`
}

// GetGroupLayout assembles the lane/slot/payload layout for a node group.
// Uses bulk queries to avoid N+1 per-slot database round trips.
func (db *DB) GetGroupLayout(groupID int64) (*GroupLayout, error) {
	children, err := db.ListChildNodes(groupID)
	if err != nil {
		return nil, err
	}

	// Collect all slot/direct-child node IDs under this group
	var allNodeIDs []int64
	laneSlots := make(map[int64][]*Node)  // laneID -> ordered slots
	slotDepths := make(map[int64]int)       // nodeID -> depth

	for _, child := range children {
		if child.NodeTypeCode == "LANE" {
			slots, _ := db.ListLaneSlots(child.ID)
			laneSlots[child.ID] = slots
			for _, slot := range slots {
				allNodeIDs = append(allNodeIDs, slot.ID)
				depth := 0
				if slot.Depth != nil {
					depth = *slot.Depth
				}
				slotDepths[slot.ID] = depth
			}
		} else if !child.IsSynthetic {
			allNodeIDs = append(allNodeIDs, child.ID)
		}
	}

	// Bulk-fetch all bins at these nodes in one query
	binsByNode := make(map[int64]*Bin)
	if len(allNodeIDs) > 0 {
		placeholders := make([]string, len(allNodeIDs))
		args := make([]any, len(allNodeIDs))
		for i, id := range allNodeIDs {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		query := fmt.Sprintf(`%s WHERE b.node_id IN (%s) ORDER BY b.id ASC`,
			binJoinQuery, strings.Join(placeholders, ", "))
		rows, err := db.Query(query, args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				bin, err := scanBin(rows)
				if err != nil {
					continue
				}
				if bin.NodeID != nil {
					// Keep only the first (oldest) bin per node
					if _, exists := binsByNode[*bin.NodeID]; !exists {
						binsByNode[*bin.NodeID] = bin
					}
				}
			}
		}
	}

	// Assemble layout from pre-fetched data
	layout := &GroupLayout{}
	for _, child := range children {
		if child.NodeTypeCode == "LANE" {
			slots := laneSlots[child.ID]
			var si []GroupSlotInfo
			for _, slot := range slots {
				s := GroupSlotInfo{NodeID: slot.ID, Name: slot.Name, Depth: slotDepths[slot.ID]}
				if bin, ok := binsByNode[slot.ID]; ok {
					s.Bin = bin
					layout.Stats.Occupied++
					if bin.ClaimedBy != nil {
						layout.Stats.Claimed++
					}
				}
				si = append(si, s)
				layout.Stats.Total++
			}
			layout.Lanes = append(layout.Lanes, GroupLaneInfo{
				Name:  child.Name,
				ID:    child.ID,
				Slots: si,
			})
		} else if !child.IsSynthetic {
			s := GroupSlotInfo{NodeID: child.ID, Name: child.Name}
			if bin, ok := binsByNode[child.ID]; ok {
				s.Bin = bin
				layout.Stats.Occupied++
				if bin.ClaimedBy != nil {
					layout.Stats.Claimed++
				}
			}
			layout.DirectNodes = append(layout.DirectNodes, s)
			layout.Stats.Total++
		}
	}

	return layout, nil
}

// DeleteNodeGroup deletes a node group hierarchy. Physical (non-synthetic)
// child nodes are unparented and returned to the flat grid. Synthetic nodes
// (the NGRP, LANE containers) are deleted.
func (db *DB) DeleteNodeGroup(grpID int64) error {
	// Collect all descendant info before starting the transaction.
	type nodeInfo struct {
		id          int64
		isSynthetic bool
	}
	var descendants []nodeInfo
	children, _ := db.ListChildNodes(grpID)
	for _, child := range children {
		grandchildren, _ := db.ListChildNodes(child.ID)
		for _, gc := range grandchildren {
			descendants = append(descendants, nodeInfo{gc.ID, gc.IsSynthetic})
		}
		descendants = append(descendants, nodeInfo{child.ID, child.IsSynthetic})
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, d := range descendants {
		if d.isSynthetic {
			tx.Exec(`DELETE FROM node_properties WHERE node_id=$1`, d.id)
			tx.Exec(`DELETE FROM node_stations WHERE node_id=$1`, d.id)
			tx.Exec(`DELETE FROM node_payloads WHERE node_id=$1`, d.id)
			tx.Exec(`DELETE FROM nodes WHERE id=$1`, d.id)
		} else {
			// Unparent physical nodes — return them to the flat grid
			tx.Exec(`UPDATE nodes SET parent_id=NULL, updated_at=NOW() WHERE id=$1`, d.id)
			tx.Exec(`DELETE FROM node_properties WHERE node_id=$1 AND key IN ('depth','role')`, d.id)
		}
	}

	// Delete the node group itself
	tx.Exec(`DELETE FROM node_properties WHERE node_id=$1`, grpID)
	tx.Exec(`DELETE FROM node_stations WHERE node_id=$1`, grpID)
	tx.Exec(`DELETE FROM node_payloads WHERE node_id=$1`, grpID)
	tx.Exec(`DELETE FROM nodes WHERE id=$1`, grpID)

	return tx.Commit()
}
