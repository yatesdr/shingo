package dispatch

import (
	"fmt"

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

// SecureStoreSlot secures a queued store order's destination slot before the
// fleet dispatch, so the scanner-replay path can never double-drop into a slot
// another store already owns (#115/#117). It resolves the order's fixed
// delivery_node and routes it through claimStoreSlot; a non-nil error means the
// slot is not (yet) ours — the caller requeues and waits, keeping its bin.
// Owner-idempotent, so a store that already reserved its slot at intake passes
// straight through on replay.
func (d *Dispatcher) SecureStoreSlot(order *orders.Order) error {
	if order.DeliveryNode == "" {
		return fmt.Errorf("store order %d has no delivery node to secure", order.ID)
	}
	node, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil || node == nil {
		return fmt.Errorf("store order %d delivery node %q not found: %w", order.ID, order.DeliveryNode, err)
	}
	return claimStoreSlot(d.db, order, node)
}
