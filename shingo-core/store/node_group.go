package store

// Stage 2D delegate file: node_group CRUD (CreateGroup / AddLane /
// DeleteGroup) lives in store/nodes/. The cross-aggregate GetGroupLayout
// stays here because the layout structs embed a concrete *bins.Bin pointer.

import (
	"fmt"
	"strings"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// CreateNodeGroup creates an empty NGRP node with the given name. Lanes and
// direct children are added separately via AddLane and reparenting.
func (db *DB) CreateNodeGroup(name string) (int64, error) {
	return nodes.CreateGroup(db.DB, name)
}

// AddLane creates a LANE node as a child of the given node group.
func (db *DB) AddLane(groupID int64, name string) (int64, error) {
	return nodes.AddLane(db.DB, groupID, name)
}

// GroupSlotInfo describes a slot in a node group layout.
type GroupSlotInfo struct {
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	Depth  int    `json:"depth"`
	Bin         *bins.Bin   `json:"bin,omitempty"`
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
// Cross-aggregate composition (nodes ↔ bins).
func (db *DB) GetGroupLayout(groupID int64) (*GroupLayout, error) {
	children, err := nodes.ListChildren(db.DB, groupID)
	if err != nil {
		return nil, err
	}

	// Collect all slot/direct-child node IDs under this group
	var allNodeIDs []int64
	laneSlots := make(map[int64][]*nodes.Node) // laneID -> ordered slots
	slotDepths := make(map[int64]int)    // nodeID -> depth

	for _, child := range children {
		if child.NodeTypeCode == "LANE" {
			slots, _ := nodes.ListLaneSlots(db.DB, child.ID)
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
	binsByNode := make(map[int64]*bins.Bin)
	if len(allNodeIDs) > 0 {
		placeholders := make([]string, len(allNodeIDs))
		args := make([]any, len(allNodeIDs))
		for i, id := range allNodeIDs {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		query := fmt.Sprintf(`%s WHERE b.node_id IN (%s) ORDER BY b.id ASC`,
			bins.BinJoinQuery, strings.Join(placeholders, ", "))
		rows, err := db.Query(query, args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				bin, err := bins.ScanBin(rows)
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
	return nodes.DeleteGroup(db.DB, grpID)
}
