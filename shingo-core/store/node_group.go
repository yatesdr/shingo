package store

import "fmt"

// CreateNodeGroup creates an empty NGRP node with the given name.
// Lanes and direct children are added separately via AddLane and drag-and-drop reparenting.
func (db *DB) CreateNodeGroup(name string) (int64, error) {
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		return 0, fmt.Errorf("NGRP node type not found")
	}
	result, err := db.Exec(db.Q(`INSERT INTO nodes (name, node_type, is_synthetic, node_type_id, capacity, enabled) VALUES (?, 'storage', 1, ?, 0, 1)`),
		name, grpType.ID)
	if err != nil {
		return 0, fmt.Errorf("create node group: %w", err)
	}
	id, _ := result.LastInsertId()
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
	result, err := db.Exec(db.Q(`INSERT INTO nodes (name, node_type, is_synthetic, node_type_id, parent_id, zone, capacity, enabled) VALUES (?, 'storage', 1, ?, ?, ?, 0, 1)`),
		name, lanType.ID, groupID, grpNode.Zone)
	if err != nil {
		return 0, fmt.Errorf("create lane: %w", err)
	}
	laneID, _ := result.LastInsertId()
	return laneID, nil
}

// GroupSlotInfo describes a slot in a node group layout.
type GroupSlotInfo struct {
	NodeID   int64            `json:"node_id"`
	Name     string           `json:"name"`
	Depth    int              `json:"depth"`
	Instance *PayloadInstance `json:"instance,omitempty"`
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

// GetGroupLayout assembles the lane/slot/instance layout for a node group.
func (db *DB) GetGroupLayout(groupID int64) (*GroupLayout, error) {
	children, err := db.ListChildNodes(groupID)
	if err != nil {
		return nil, err
	}

	layout := &GroupLayout{}

	for _, child := range children {
		if child.NodeTypeCode == "LANE" {
			slots, _ := db.ListLaneSlots(child.ID)
			var si []GroupSlotInfo
			for _, slot := range slots {
				depth, _ := db.GetSlotDepth(slot.ID)
				s := GroupSlotInfo{NodeID: slot.ID, Name: slot.Name, Depth: depth}
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
			layout.Lanes = append(layout.Lanes, GroupLaneInfo{
				Name:  child.Name,
				ID:    child.ID,
				Slots: si,
			})
		} else if !child.IsSynthetic {
			// Direct physical child of the group
			s := GroupSlotInfo{NodeID: child.ID, Name: child.Name}
			instances, _ := db.ListInstancesByNode(child.ID)
			if len(instances) > 0 {
				s.Instance = instances[0]
				layout.Stats.Occupied++
				if instances[0].ClaimedBy != nil {
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
	// Collect all descendant info before starting the transaction to avoid
	// deadlocking on SQLite's single-connection pool.
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
			tx.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=?`), d.id)
			tx.Exec(db.Q(`DELETE FROM node_stations WHERE node_id=?`), d.id)
			tx.Exec(db.Q(`DELETE FROM node_payload_styles WHERE node_id=?`), d.id)
			tx.Exec(db.Q(`DELETE FROM nodes WHERE id=?`), d.id)
		} else {
			// Unparent physical nodes — return them to the flat grid
			tx.Exec(db.Q(`UPDATE nodes SET parent_id=NULL, updated_at=datetime('now','localtime') WHERE id=?`), d.id)
			tx.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=? AND key IN ('depth','role')`), d.id)
		}
	}

	// Delete the node group itself
	tx.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=?`), grpID)
	tx.Exec(db.Q(`DELETE FROM node_stations WHERE node_id=?`), grpID)
	tx.Exec(db.Q(`DELETE FROM node_payload_styles WHERE node_id=?`), grpID)
	tx.Exec(db.Q(`DELETE FROM nodes WHERE id=?`), grpID)

	return tx.Commit()
}
