package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// ── THE REFUSAL NAMES ITS OWN CAUSE ───────────────────────────────────────
//
// ReserveStorageDropoff can refuse for three reasons that look identical to a
// caller reading a bare error, and they are three different waits:
//
//	CONTENDED    a carrier stands in the slot, or another order reserved it
//	             first. Clears when that carrier leaves. Genuine backpressure.
//	UNRESOLVED   the destination still names a GROUP and no child of it can
//	             take the bin. Clears when the group frees up — and the operator
//	             has to look at the group, not at any one slot.
//	UNREADABLE   a database read failed. Clears when the database answers, and
//	             NOTHING about the slot is known.
//
// Both scanner call sites classified this with the same four hand-written lines
// — unresolved-group if IsSyntheticUnresolved, otherwise slot-contended — so
// every hard read failure parked as "the destination slot is contended". That
// sends an operator to a slot that is probably empty, to wait for a carrier that
// is not there, on a wait that will not clear until a database does.
//
// The decision moves to the arm that made it. Callers read StorageDropoff.Cause
// and write it; they no longer re-derive it, and a fourth refusal shape added
// here cannot be silently absorbed into "contended".
//
// The two sentinels are wrapped rather than returned bare so the message keeps
// the node, the count and the order id it always carried — the log line is what
// somebody reads at three in the morning, and it is not made better by being
// shorter.

// ErrSlotContended marks a refusal caused by the slot being taken — a carrier
// standing in it, or another order's reservation. It is the only shape that is
// honest backpressure.
var ErrSlotContended = errors.New("store slot contended")

// ErrSlotUnreadable marks a refusal caused by a failed read rather than by any
// fact about the slot.
var ErrSlotUnreadable = errors.New("store slot state unreadable")

// errRead wraps a read failure so classification survives the message wrap. A
// nil err still yields a non-nil sentinel: the two call sites that use it both
// fire on `err != nil || node == nil`, and a missing node with no error is
// exactly as unreadable as an error is.
func errRead(err error) error {
	if err == nil {
		return ErrSlotUnreadable
	}
	return fmt.Errorf("%w: %v", ErrSlotUnreadable, err)
}

// StorageDropoff is one ReserveStorageDropoff decision: where the order is
// actually going, or why it cannot go yet and under which cause it parks.
//
// Node is non-nil exactly when Err is nil, and it is the SETTLED destination —
// a group resolved to a child, a store re-aimed off a dug lane. It is the only
// node a caller may use afterwards; the one it read before calling is stale.
type StorageDropoff struct {
	Node  *nodes.Node
	Cause QueueCause
	Err   error
}

// Refused reports whether the reserve failed. Callers park on true.
func (r StorageDropoff) Refused() bool { return r.Err != nil }

// causeForStorageDropoff maps a refusal to the cause its wait parks under.
//
// The default is the UNDETERMINED reading, not the contended one, and that
// direction is the whole point: an unrecognised refusal is a refusal nobody has
// classified, and calling it contention is a confident claim the code has not
// earned. CauseCapacityCheckFailed is the existing member of that family.
func causeForStorageDropoff(err error) QueueCause {
	switch {
	case IsSyntheticUnresolved(err):
		return CauseNGRPResolve
	case errors.Is(err, ErrSlotContended):
		return CauseStoreSlotContended
	default:
		return CauseCapacityCheckFailed
	}
}

// claimStoreSlot atomically secures `node` as a store order's destination slot
// (#115/#117). Two concurrent stores that resolve the same destination used to
// both pass a capacity READ and both dispatch, dropping two bins into one
// single-bin node. This routes the store through the reservation layer instead:
// a PENDING slot reservation is exclusive per node (one order at a time), so the
// loser gets a non-nil error and its caller requeues, keeping its bin.
//
// Deliberately reserve-ONLY (no ConfirmSlotClaim hard claim): a hard claim sets
// nodes.claimed_by, which makes the node look taken to the sibling stores
// and would terminal-fail the ones that must instead WAIT (the changeover
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
		// UNREADABLE IS NOT CONTENDED. The caller parks either way, but under
		// different causes and with different releasers: a contended slot clears
		// when a carrier leaves, and an unreadable one clears when the database
		// answers. Collapsing them told an operator to go and look at a slot that
		// was probably empty.
		return fmt.Errorf("store order %d: count bins at %s: %w", order.ID, node.Name, errRead(err))
	}
	if cnt > 0 {
		return fmt.Errorf("%w: store slot %s occupied (%d bin(s))", ErrSlotContended, node.Name, cnt)
	}
	// Cross-order exclusivity via a pending slot reservation. Owner-aware: reuse
	// one this order already holds so a replay tick doesn't self-conflict.
	if orderHoldsSlotReservation(db, order.ID, node.ID) {
		return nil
	}
	if err := db.ReserveSlot(node.ID, order.ID); err != nil {
		// ErrReservationConflict here means another store already reserved this
		// slot — the caller requeues and waits, and that IS contention. A hard DB
		// error is likewise not safe to dispatch on, but it is a different wait
		// with a different releaser, so the two are tagged apart rather than
		// surfaced as one.
		if errors.Is(err, reservations.ErrReservationConflict) {
			return fmt.Errorf("%w: reserve store slot %s for order %d: %v",
				ErrSlotContended, node.Name, order.ID, err)
		}
		return fmt.Errorf("reserve store slot %s for order %d: %w", node.Name, order.ID, errRead(err))
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
// plain order exclusively occupies — the node fact that drives the
// reservation. Broader than isConcreteStorageDropoff on purpose: a store's
// destination is a standalone STOR-typed node (snt.code='STOR'), which is
// frequently top-level (ParentID == nil) and so
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
		return fmt.Errorf("plain order %d delivery node %q not found: %w", order.ID, order.DeliveryNode, errRead(err))
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
//
// IT RETURNS THE DESTINATION, and that is the fix. It also SETTLES that
// destination — resolving a group to a child, re-aiming off a dug lane — and it
// used to do so silently. Callers read the node BEFORE calling and never
// re-read, so a settled order carried the new name on its row and the OLD node
// into the lane declaration, the slot confirm, and the transport plan: the
// record was right and the robot still drove into the dug lane. Nil error ⇒ the
// returned node is where the order is going, and the only one a caller may use.
func (d *Dispatcher) ReserveStorageDropoff(order *orders.Order) StorageDropoff {
	refuse := func(err error) StorageDropoff {
		return StorageDropoff{Cause: causeForStorageDropoff(err), Err: err}
	}
	if err := d.resolveSyntheticDropoff(order); err != nil {
		return refuse(err)
	}
	d.redirectStoreOffDugLane(order)
	if err := reserveStorageDropoff(d.db, order); err != nil {
		return refuse(err)
	}
	node, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		return refuse(fmt.Errorf("read settled destination %q for order %d: %w",
			order.DeliveryNode, order.ID, errRead(err)))
	}
	if node == nil {
		return refuse(fmt.Errorf("settled destination %q for order %d does not exist: %w",
			order.DeliveryNode, order.ID, ErrSlotUnreadable))
	}
	return StorageDropoff{Node: node}
}

// SyntheticUnresolved is the refusal when a destination still names a synthetic
// node and no concrete child can be had — group full, every lane dug, or no
// children.
//
// It is a WAIT, not a failure: intake defers resolution deliberately when a
// group is full and queues the order on the promise dispatch resolves it, so
// "not yet" is legitimate. Proceeding is not — that is what produced the creates
// the fleet rejected with 50001.
type SyntheticUnresolved struct {
	OrderID int64
	Group   string
	Err     error
}

func (s SyntheticUnresolved) Error() string {
	return fmt.Sprintf("order %d is aimed at %s, which names a set of positions rather than one, "+
		"and no child of it can take the bin: %v", s.OrderID, s.Group, s.Err)
}

func (s SyntheticUnresolved) Unwrap() error { return s.Err }

// IsSyntheticUnresolved reports whether err is an unresolved-group wait, so a
// caller can park under the cause that names it rather than one that blames the
// slot layer for a resolution that never ran.
func IsSyntheticUnresolved(err error) bool {
	var su SyntheticUnresolved
	return errors.As(err, &su)
}

// resolveSyntheticDropoff keeps the promise intake makes when it defers.
//
// The deferral is made at three sites and was kept at one: intake leaves the
// group name on the order and queues it, planning_service re-resolves on its own
// path, and the scanner had no resolver at all — GetNodeByDotName FINDS a group,
// because it is a real row, so there was no error to catch. HK: 26 such orders
// since June, none completed.
//
// It lives INSIDE ReserveStorageDropoff because that is the one call every plain
// scanner path already makes between reading the destination and dispatching. A
// separate step the scanner must remember is the shape of the bug, not the fix.
//
// Keyed on IsSynthetic, not IsSynthetic && NGRP: intake uses the broad predicate
// and LANE is seeded synthetic too, so the narrow one would be born carrying the
// divergence this removes. Resolve handles both — NGRP via GroupResolver, LANE
// via resolveStore over its concrete children.
func (d *Dispatcher) resolveSyntheticDropoff(order *orders.Order) error {
	if d.resolver == nil || order == nil || order.DeliveryNode == "" {
		return nil
	}
	node, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil || node == nil || !node.IsSynthetic {
		// A read error is left to the settle-read below, which reports it with
		// the order id attached rather than swallowing it here.
		return nil
	}
	result, rErr := d.resolver.Resolve(node, binresolver.ResolveModeStore, order.PayloadCode, nil, digAskerFor(order))
	if rErr != nil || result == nil || result.Node == nil {
		if rErr == nil {
			rErr = fmt.Errorf("resolver returned no node")
		}
		return SyntheticUnresolved{OrderID: order.ID, Group: order.DeliveryNode, Err: rErr}
	}
	if uErr := d.db.UpdateOrderDeliveryNode(order.ID, result.Node.Name); uErr != nil {
		return fmt.Errorf("order %d resolved %s to %s but delivery_node could not be written: %w",
			order.ID, order.DeliveryNode, result.Node.Name, uErr)
	}
	d.dbg("store: order %d resolved group %s -> %s at dispatch", order.ID, order.DeliveryNode, result.Node.Name)
	order.DeliveryNode = result.Node.Name
	return nil
}

// redirectStoreOffDugLane re-aims a store whose destination lane has been taken
// over by a dig since the destination was chosen.
//
// SELECTION ALREADY DIVERTS off dig-locked lanes (ListChildNodesUnlocked filters
// them out of the store resolver's candidate pool), so this case cannot arise for
// an order that is choosing its destination NOW. It arises for one that chose
// EARLIER: intake resolved a group to a concrete slot, the order queued behind
// inventory or capacity, and a dig took that lane while it waited. Nothing
// re-selected, so admission refused it at dispatch and it sat out the whole
// excavation with a sibling lane standing empty.
//
// The destination is a plan like any other, and a plan that has stopped being
// reachable gets redone. Re-selecting costs one resolver call on a path already
// doing database work; waiting costs the length of a dig.
//
// ONLY THE SLOT RESERVATION IS RELEASED. The bin hold is never touched, and the
// asymmetry is deliberate rather than incidental: the keep-your-holds discipline
// exists because the finders exclude pending-reserved bins owner-blind, so an
// order that dropped its bin hold to re-shop would double-source. Slots were
// never that hazard — the slot resolver is owner-aware — so releasing one to pick
// a better lane costs nothing and strands nothing.
//
// IT LEAVES AN OPERATOR'S CHOICE ALONE. An order stamped no-demand had its
// destination named by a human at a door (the bin move, the spot order), and
// re-aiming that is not a recalculation, it is Core overruling somebody. Those
// park and wait, which is the honest outcome for a destination Core did not pick.
//
// Best-effort throughout: every doubt leaves the order exactly as it was, and the
// worst case is the behaviour that existed before this function.
func (d *Dispatcher) redirectStoreOffDugLane(order *orders.Order) {
	if d.resolver == nil || order == nil || order.OriginClass == protocol.OriginClassNoDemand {
		return
	}
	if !isStorageDropoff(d.db, order.DeliveryNode) {
		return
	}
	node, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil || node == nil {
		return
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return // not a lane slot, or a lane with no group to re-select within
	}
	if !d.laneLock.IsLocked(lane.ID) {
		return // the ordinary case: the destination is still fine
	}
	group, err := d.db.GetNode(*lane.ParentID)
	if err != nil || group == nil {
		return
	}

	// The resolver's candidate read drops dig-locked lanes, so this either comes
	// back with somewhere else or comes back empty — and empty means every lane in
	// the group is locked or full, which is the existing park.
	result, err := d.resolver.Resolve(group, binresolver.ResolveModeStore, order.PayloadCode, nil, digAskerFor(order))
	if err != nil || result == nil || result.Node == nil || result.Node.ID == node.ID {
		d.dbg("store: order %d is aimed at %s in dug lane %s and the group has nowhere else — waiting",
			order.ID, node.Name, lane.Name)
		return
	}

	// Drop the hold on the slot we are no longer going to, so it is available to
	// whoever can use it. PENDING only — a confirmed slot means this order is
	// already dispatching and is not re-aimable.
	if rErr := d.db.ReleaseSlotReservation(node.ID, order.ID); rErr != nil {
		d.dbg("store: order %d could not release its hold on %s (%v) — leaving it aimed there",
			order.ID, node.Name, rErr)
		return
	}
	if uErr := d.db.UpdateOrderDeliveryNode(order.ID, result.Node.Name); uErr != nil {
		log.Printf("dispatch: order %d re-aimed off dug lane %s but delivery_node could not be "+
			"written (%v) — it keeps the old destination and waits", order.ID, lane.Name, uErr)
		return
	}
	order.DeliveryNode = result.Node.Name
	log.Printf("dispatch: order %d re-aimed from %s to %s — a dig took lane %s after the destination "+
		"was chosen, and the group had somewhere else", order.ID, node.Name, result.Node.Name, lane.Name)
}

// ConfirmForDispatch is the Rule-1 confirm-at-dispatch step for the plain
// (single-transport) path: it hard-claims BOTH the destination slot (if a concrete
// storage dropoff) AND the source bin, in one logical step, immediately before the
// fleet call. This is the plain analog of the complex path's confirmComplexPlan
// (slots-then-bins, owner-idempotent, seatbelted). The slot and bin were SOFT-held
// (pending reservations) by ReserveStorageDropoff and ReserveForDispatch while the
// order waited; this flips both to confirmed and writes the hard claimed_by columns.
//
// Order: slot FIRST, then bin (slots-before-bins, matching the complex path, to
// keep a slot↔bin cross-type claim cycle from forming). Each leg is owner-idempotent:
// a resource already hard-claimed by THIS order (a prior tick that crashed between
// the two legs) is confirmed-in-place rather than re-claimed. A failure returns a
// non-nil error so the caller parks the order in sourcing under claim_failed and
// retries next tick — the soft holds are retained, so re-entry re-confirms.
//
// A non-storage dropoff (line/consume point) skips the slot leg entirely.
func (d *Dispatcher) ConfirmForDispatch(order *orders.Order, binID int64, sourceNode, destNode *nodes.Node) error {
	// Slot leg: only a concrete storage dropoff is hard-claimed. A line/consume dest
	// has no slot reservation to confirm.
	if isStorageDropoff(d.db, order.DeliveryNode) && destNode != nil {
		if err := d.confirmDropoffSlot(order, destNode); err != nil {
			return fmt.Errorf("confirm slot %s for order %d: %w", order.DeliveryNode, order.ID, err)
		}
	}
	// Bin leg: confirm the soft-held bin reservation → hard claim + confirmed, one tx.
	if err := d.binManifest.ConfirmClaim(binID, order.ID, order.RemainingUOP); err != nil {
		return fmt.Errorf("confirm bin %d for order %d: %w", binID, order.ID, err)
	}
	return nil
}

// confirmDropoffSlot hard-claims the destination slot for a storage dropoff at
// dispatch. Owner-idempotent: a slot already claimed by THIS order (a prior confirm
// that crashed before the bin leg) is confirmed-in-place via ConfirmSlotReservation;
// otherwise ConfirmSlotClaim runs the reservation-guarded hard claim (claim +
// confirm in one tx, NOT EXISTS bins seatbelt). Mirrors the complex path's per-slot
// confirm (allocator.confirmComplexPlan).
func (d *Dispatcher) confirmDropoffSlot(order *orders.Order, destNode *nodes.Node) error {
	if destNode.ClaimedBy != nil && *destNode.ClaimedBy == order.ID {
		// Already ours — confirm any still-pending reservation in place, no re-claim.
		return d.db.ConfirmSlotReservation(destNode.ID, order.ID)
	}
	return d.db.ConfirmSlotClaim(destNode.ID, order.ID)
}

// RefuseStorageDropoffForTest builds the refusal a real ReserveStorageDropoff
// would return for err, so a test seam standing in for the Dispatcher classifies
// through the SAME mapper production uses.
//
// EXPORTED FOR TESTS AND SAID SO IN THE NAME. A stub that picked its own cause
// could not tell a test anything about which cause a given failure earns — and
// "which cause does this failure earn" is precisely the question a hard database
// error being parked as `store-slot-contended` got wrong. Routing the seam
// through causeForStorageDropoff makes the fulfillment tests assertions about
// production behaviour rather than about the stub.
func RefuseStorageDropoffForTest(err error) StorageDropoff {
	return StorageDropoff{Cause: causeForStorageDropoff(err), Err: err}
}
