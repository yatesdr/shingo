package store

// Stage 2D delegate file: node CRUD lives in store/nodes/. This file
// preserves the *store.DB method surface so external callers don't need to
// change.

import (
	"fmt"

	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

func (db *DB) CreateNode(n *nodes.Node) error        { return nodes.Create(db.DB, n) }
func (db *DB) UpdateNode(n *nodes.Node) error        { return nodes.Update(db.DB, n) }
func (db *DB) DeleteNode(id int64) error             { return nodes.Delete(db.DB, id) }
func (db *DB) GetNode(id int64) (*nodes.Node, error) { return nodes.Get(db.DB, id) }
func (db *DB) GetNodeByName(name string) (*nodes.Node, error) {
	return nodes.GetByName(db.DB, name)
}
func (db *DB) GetNodeByDotName(name string) (*nodes.Node, error) {
	return nodes.GetByDotName(db.DB, name)
}
func (db *DB) GetRootNode(nodeID int64) (*nodes.Node, error) { return nodes.GetRoot(db.DB, nodeID) }
func (db *DB) ListNodes() ([]*nodes.Node, error)             { return nodes.List(db.DB) }
func (db *DB) ListChildNodes(parentID int64) ([]*nodes.Node, error) {
	return nodes.ListChildren(db.DB, parentID)
}
func (db *DB) SetNodeParent(nodeID, parentID int64) error {
	return nodes.SetParent(db.DB, nodeID, parentID)
}
func (db *DB) ClearNodeParent(nodeID int64) error { return nodes.ClearParent(db.DB, nodeID) }

// (D47) db.ClaimSlot deleted with nodes.ClaimSlot — the live slot-claim path is
// ConfirmSlotClaim below (reserve → guarded claim → confirm, one tx).

// ConfirmSlotClaim commits an ALREADY-RESERVED slot to a hard claim and confirms
// the reservation (pending→confirmed) in ONE transaction — the slot mirror of
// BinManifestService.claimAndConfirm (D46). The pending slot reservation was placed
// earlier by the reserve reconcile (commit 4); ConfirmSlotClaim does NOT acquire. It
// runs the seatbelted, owner-idempotent ClaimSlotTx (claimed_by IS NULL OR =order,
// NOT EXISTS bins, EXISTS a pending slot reservation) then ConfirmSlot — both writes
// commit together or neither, so a transient failure between them can never leave the
// slot claimed with its reservation stuck pending. No production callers until commit
// 4 wires it into confirmComplexPlan.
func (db *DB) ConfirmSlotClaim(nodeID, orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if err := nodes.ClaimSlotTx(tx, nodeID, orderID); err != nil {
		return err
	}
	if err := reservations.ConfirmSlot(tx, orderID, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReserveSlot places a pending slot reservation on nodeID for orderID — the slot
// dual of BinManifestService.ReserveForDispatch. Returns
// reservations.ErrReservationConflict when another order already holds an active
// slot reservation on the node (the reconcile treats it as a lost race / revert).
func (db *DB) ReserveSlot(nodeID, orderID int64) error {
	return reservations.AcquireSlot(db.DB, orderID, nodeID, "reserveComplexPlan")
}

// ReleaseSlotReservation drops a PENDING slot reservation the order no longer needs
// (a stray left by a re-resolution) — the plain, uncoupled release (no claim yet).
func (db *DB) ReleaseSlotReservation(nodeID, orderID int64) error {
	return reservations.ReleaseSlot(db.DB, orderID, nodeID)
}

// ConfirmSlotReservation flips a slot reservation pending→confirmed WITHOUT
// re-claiming — the slot mirror of BinManifestService.ConfirmHeldReservation, for
// the crash-replay case where the slot is already claimed_by the order but its
// reservation is still pending.
func (db *DB) ConfirmSlotReservation(nodeID, orderID int64) error {
	return reservations.ConfirmSlot(db.DB, orderID, nodeID)
}

// ReleaseSlotClaim is the coupled inverse of ConfirmSlotClaim: it clears the slot's
// claimed_by AND releases its reservation in one tx — for a CONFIRMED slot the
// reserve reconcile abandons (a re-resolution moved the dropoff). The slot dual of
// ReleaseClaimForBin. Idempotent.
func (db *DB) ReleaseSlotClaim(nodeID, orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE id=$1 AND claimed_by=$2`, nodeID, orderID); err != nil {
		return err
	}
	if err := reservations.ReleaseSlot(tx, orderID, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

// UnclaimSlot releases a single slot claim.
func (db *DB) UnclaimSlot(nodeID int64) error { return nodes.UnclaimSlot(db.DB, nodeID) }

// UnclaimOrderSlots releases all slot claims held by an order. Mirrors
// UnclaimOrderBins and is called from the same terminal cleanup hooks.
func (db *DB) UnclaimOrderSlots(orderID int64) error { return nodes.UnclaimOrderSlots(db.DB, orderID) }

// ReparentNode moves a node into a new parent (or removes it from a parent).
// When adopting into a lane, it sets the depth based on position. When
// orphaning, it clears depth and role properties.
func (db *DB) ReparentNode(nodeID int64, parentID *int64, position int) error {
	return nodes.Reparent(db.DB, nodeID, parentID, position)
}

// ReorderLaneSlots updates depth for all slots in a lane based on the
// provided ordered list of node IDs.
func (db *DB) ReorderLaneSlots(laneID int64, orderedNodeIDs []int64) error {
	return nodes.ReorderLaneSlots(db.DB, laneID, orderedNodeIDs)
}
