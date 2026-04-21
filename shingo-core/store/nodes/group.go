package nodes

import (
	"database/sql"
	"fmt"

	"shingocore/store/internal/helpers"
)

// CreateGroup creates an empty NGRP node with the given name.
// Lanes and direct children are added separately via AddLane and reparenting.
func CreateGroup(db *sql.DB, name string) (int64, error) {
	grpType, err := GetTypeByCode(db, "NGRP")
	if err != nil {
		return 0, fmt.Errorf("NGRP node type not found")
	}
	id, err := helpers.InsertID(db, `INSERT INTO nodes (name, is_synthetic, node_type_id, enabled) VALUES ($1, true, $2, true) RETURNING id`,
		name, grpType.ID)
	if err != nil {
		return 0, fmt.Errorf("create node group: %w", err)
	}
	return id, nil
}

// AddLane creates a LANE node as a child of the given node group.
func AddLane(db *sql.DB, groupID int64, name string) (int64, error) {
	grpNode, err := Get(db, groupID)
	if err != nil {
		return 0, fmt.Errorf("node group not found: %w", err)
	}
	lanType, err := GetTypeByCode(db, "LANE")
	if err != nil {
		return 0, fmt.Errorf("LANE node type not found")
	}
	laneID, err := helpers.InsertID(db, `INSERT INTO nodes (name, is_synthetic, node_type_id, parent_id, zone, enabled) VALUES ($1, true, $2, $3, $4, true) RETURNING id`,
		name, lanType.ID, groupID, grpNode.Zone)
	if err != nil {
		return 0, fmt.Errorf("create lane: %w", err)
	}
	return laneID, nil
}

// DeleteGroup deletes a node group hierarchy. Physical (non-synthetic)
// child nodes are unparented and returned to the flat grid. Synthetic
// nodes (the NGRP, LANE containers) are deleted.
//
// The GroupLayout/GroupSlotInfo/GroupLaneInfo/GroupStats types used for
// rendering live at the outer store/ level because they carry a concrete
// *bins.Bin pointer; composing them requires crossing the bins aggregate.
func DeleteGroup(db *sql.DB, grpID int64) error {
	type nodeInfo struct {
		id          int64
		isSynthetic bool
	}
	var descendants []nodeInfo
	children, _ := ListChildren(db, grpID)
	for _, child := range children {
		grandchildren, _ := ListChildren(db, child.ID)
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
			tx.Exec(`UPDATE nodes SET parent_id=NULL, updated_at=NOW() WHERE id=$1`, d.id)
			tx.Exec(`DELETE FROM node_properties WHERE node_id=$1 AND key IN ('depth','role')`, d.id)
		}
	}

	tx.Exec(`DELETE FROM node_properties WHERE node_id=$1`, grpID)
	tx.Exec(`DELETE FROM node_stations WHERE node_id=$1`, grpID)
	tx.Exec(`DELETE FROM node_payloads WHERE node_id=$1`, grpID)
	tx.Exec(`DELETE FROM nodes WHERE id=$1`, grpID)

	return tx.Commit()
}
