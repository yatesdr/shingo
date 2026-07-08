package dispatch

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// claimStoreSlot atomically secures `node` as a store order's destination slot
// (#115/#117). Two concurrent stores that resolve the same destination used to
// both pass a capacity READ and both dispatch, dropping two bins into one
// single-bin node. This routes the store through the reservation layer instead:
// a PENDING slot reservation is exclusive per node (one order at a time), so the
// loser gets a non-nil error and its caller requeues, keeping its bin.
//
// Deliberately reserve-ONLY (no ConfirmSlotClaim hard claim): a hard claim sets
// nodes.claimed_by, which drops the node out of FindStorageDestination's pool
// and would terminal-fail sibling stores that must instead WAIT (the changeover
// N-store polite-wait — plants routinely issue more store orders than there are
// free slots, and the extras hold their bins until a slot frees). The pending
// reservation keeps the node findable, so a sibling that resolves the same slot
// loses the reservation race and requeues rather than failing. The reservation
// is held for the order's lifetime (owner-liveness) and released on completion;
// it is never age-reaped.
//
// An occupancy guard (one-bin-per-node) refuses to drop into a slot that already
// holds a bin — the reservation covers store-vs-store, this covers a slot filled
// between select and dispatch (or a fixed delivery_node whose slot filled while
// the order waited). Neither seatbelt (bin demoted-CAS, slot NOT-EXISTS-bins) is
// touched.
//
// Owner-idempotent / replay-safe: a reservation this order already holds is
// reused (the scanner re-runs this every dispatch tick), so re-acquiring never
// self-conflicts.
func claimStoreSlot(db *store.DB, order *orders.Order, node *nodes.Node) error {
	if node == nil {
		return fmt.Errorf("store order %d: nil destination node", order.ID)
	}
	// Occupancy guard: never dispatch a store into an occupied single-bin node.
	cnt, err := db.CountBinsByNode(node.ID)
	if err != nil {
		return fmt.Errorf("store order %d: count bins at %s: %w", order.ID, node.Name, err)
	}
	if cnt > 0 {
		return fmt.Errorf("store slot %s occupied (%d bin(s))", node.Name, cnt)
	}
	// Cross-order exclusivity via a pending slot reservation. Owner-aware: reuse
	// one this order already holds so a replay tick doesn't self-conflict.
	if orderHoldsSlotReservation(db, order.ID, node.ID) {
		return nil
	}
	if err := db.ReserveSlot(node.ID, order.ID); err != nil {
		// ErrReservationConflict here means another store already reserved this
		// slot — the caller requeues and waits. A hard DB error is likewise not
		// safe to dispatch on, so surface both.
		return fmt.Errorf("reserve store slot %s for order %d: %w", node.Name, order.ID, err)
	}
	return nil
}

// orderHoldsSlotReservation reports whether orderID already holds a slot
// reservation on nodeID (the owner-aware reuse the replay path needs).
func orderHoldsSlotReservation(db *store.DB, orderID, nodeID int64) bool {
	rows, err := db.ListReservationsByOrder(orderID)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.Kind == reservations.KindSlot && r.NodeID == nodeID {
			return true
		}
	}
	return false
}

// isStorageDropoff reports whether a delivery node is a concrete storage slot a
// plain order exclusively occupies — the node fact that drives the ★ Stage-3
// reservation. Broader than isConcreteStorageDropoff on purpose: a store's
// destination is a standalone STOR-typed node (FindStorageDestination targets
// snt.code='STOR'), which is frequently top-level (ParentID == nil) and so
// isConcreteStorageDropoff-false — gating C2's reserve on the bare predicate
// would stop reserving the store's own destination. This union covers the
// standalone STOR node AND deep-lane / NGRP-child slots; it excludes lines and
// consume points (no STOR type, no LANE/NGRP parent), which are never reserved.
func isStorageDropoff(db *store.DB, deliveryNode string) bool {
	if deliveryNode == "" {
		return false
	}
	node, err := db.GetNodeByDotName(deliveryNode)
	if err != nil || node == nil || node.IsSynthetic {
		return false
	}
	if node.NodeTypeCode == protocol.NodeClassSTOR {
		return true
	}
	return isConcreteStorageDropoff(db, deliveryNode)
}

// reserveStorageDropoff is the node-driven Stage-3 generalization of C2's
// per-store claim to EVERY plain family (store, move, retrieve, retrieve_empty):
// after the plain-path full gate passes, if the dropoff is a concrete storage
// slot (isStorageDropoff) it is reserved RESERVE-ONLY via claimStoreSlot, else
// it is a no-op (lines/consume points reserve nothing). This closes the
// move-to-storage race that previously had only a CheckDropoffCapacity read.
// The gating is node-driven, NOT type-driven; the inner claimStoreSlot stays the
// reserve-only exclusivity primitive (never a hard claim). No-op ⇒ nil.
func reserveStorageDropoff(db *store.DB, order *orders.Order) error {
	if !isStorageDropoff(db, order.DeliveryNode) {
		return nil // line / consume point / no dest — nothing to reserve
	}
	node, err := db.GetNodeByDotName(order.DeliveryNode)
	if err != nil || node == nil {
		return fmt.Errorf("plain order %d delivery node %q not found: %w", order.ID, order.DeliveryNode, err)
	}
	return claimStoreSlot(db, order, node)
}

// ReserveStorageDropoff is the Dispatcher-surface (interface) entrypoint the
// scanner uses to node-drive the plain-path destination reserve before a fleet
// dispatch — the Stage-3 generalization of the former SecureStoreSlot (which
// only served the store family). A non-nil error means the slot is not (yet)
// ours — the caller requeues and waits, keeping its bin. Owner-idempotent, so a
// store/move that already reserved its slot at intake passes straight through on
// replay; a no-op for non-storage dropoffs.
func (d *Dispatcher) ReserveStorageDropoff(order *orders.Order) error {
	return reserveStorageDropoff(d.db, order)
}
