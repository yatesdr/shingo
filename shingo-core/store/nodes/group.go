package nodes

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
)

// CreateGroup creates an empty NGRP node with the given name.
// Lanes and direct children are added separately via AddLane and reparenting.
func CreateGroup(db *sql.DB, name string) (int64, error) {
	grpType, err := GetTypeByCode(db, protocol.NodeClassNGRP)
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
	lanType, err := GetTypeByCode(db, protocol.NodeClassLANE)
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
	// Propagate ListChildren errors: a transient failure here silently yields an
	// empty descendant set, so DeleteGroup would delete only the group root and
	// commit successfully — orphaning the physical children (never unparented)
	// and synthetic descendants (never deleted). That partial outcome is NOT
	// caught by the transaction below (no statement errors), so it must be
	// guarded here.
	children, err := ListChildren(db, grpID)
	if err != nil {
		return fmt.Errorf("list children of group %d: %w", grpID, err)
	}
	for _, child := range children {
		grandchildren, err := ListChildren(db, child.ID)
		if err != nil {
			return fmt.Errorf("list children of node %d: %w", child.ID, err)
		}
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

	// exec runs one statement and returns a labelled error on failure so we roll
	// back at the first problem. Postgres already aborts the whole transaction on
	// any statement error (so a partial delete can't commit), but checking each
	// error surfaces *which* statement failed instead of the generic
	// "commit unexpectedly resulted in rollback" the bare Commit would return.
	exec := func(label, query string, args ...any) error {
		if _, err := tx.Exec(query, args...); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}

	for _, d := range descendants {
		if d.isSynthetic {
			if err := exec("delete synthetic node properties", `DELETE FROM node_properties WHERE node_id=$1`, d.id); err != nil {
				return err
			}
			if err := exec("delete synthetic node stations", `DELETE FROM node_stations WHERE node_id=$1`, d.id); err != nil {
				return err
			}
			if err := exec("delete synthetic node payloads", `DELETE FROM node_payloads WHERE node_id=$1`, d.id); err != nil {
				return err
			}
			if err := exec("delete synthetic node", `DELETE FROM nodes WHERE id=$1`, d.id); err != nil {
				return err
			}
		} else {
			if err := exec("unparent physical node", `UPDATE nodes SET parent_id=NULL, updated_at=NOW() WHERE id=$1`, d.id); err != nil {
				return err
			}
			if err := exec("clear physical node depth/role", `DELETE FROM node_properties WHERE node_id=$1 AND key IN ('depth','role')`, d.id); err != nil {
				return err
			}
		}
	}

	if err := exec("delete group properties", `DELETE FROM node_properties WHERE node_id=$1`, grpID); err != nil {
		return err
	}
	if err := exec("delete group stations", `DELETE FROM node_stations WHERE node_id=$1`, grpID); err != nil {
		return err
	}
	if err := exec("delete group payloads", `DELETE FROM node_payloads WHERE node_id=$1`, grpID); err != nil {
		return err
	}
	if err := exec("delete group node", `DELETE FROM nodes WHERE id=$1`, grpID); err != nil {
		return err
	}

	return tx.Commit()
}
