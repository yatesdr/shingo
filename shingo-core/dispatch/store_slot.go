package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
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

// causeForGateRebind names why a release-time re-bind refused, from the typed
// error the callee already produced.
//
// ── IT USED TO STAMP ONE CAUSE OVER EVERY ANSWER ──────────────────────────
//
// Both release arms wrote CauseGateRebindUnavailable on ANY re-bind error. That
// cause means one specific thing — "this order's bin has no slot in this lane to
// rebind to" — and the callee already distinguishes the others properly, because
// they END DIFFERENTLY. claimStoreSlot says so in as many words: "a contended
// slot clears when a carrier leaves, and an unreadable one clears when the
// database answers. Collapsing them told an operator to go and look at a slot
// that was probably empty." The caller then collapsed them anyway, into a third
// name that describes neither.
//
// MEASURED, 12c snapshot: the specific answer survived only in the reservation
// layer's error. The ORDER ROW — what a board renders and what a forensic query
// groups by — carried the generic one.
//
// ── WHY IT MAPS DIRECTLY RATHER THAN DELEGATING ───────────────────────────
//
// causeForStorageDropoff is its sibling and folds the unreadable case into its
// CauseCapacityCheckFailed default, which is right THERE — that site's default
// is the undetermined answer for a capacity decision. Here the undetermined
// answer is "the re-bind failed", and a failed READ has its own name already
// (CauseReadFailed: "NOT lane-busy, and the separation is the point — during a
// database outage dozens of orders park at once, and a surface that renders that
// as congestion sends someone to look at the wrong thing"). Delegating and then
// re-mapping the default would launder a classified error back into an
// unclassified one.
//
// The default arm is CauseGateRebindUnavailable, and it is honest: it is reached
// when FindStoreSlotInLaneExcluding found nothing at all, which is the refusal
// that cause was written for.
func causeForGateRebind(err error) QueueCause {
	switch {
	case err == nil:
		return CauseGateRebindUnavailable
	case errors.Is(err, ErrSlotContended):
		return CauseStoreSlotContended
	case errors.Is(err, ErrSlotUnreadable):
		return CauseReadFailed
	default:
		return CauseGateRebindUnavailable
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
	return claimStoreSlotAs(db, order, node, false)
}

// claimStoreSlotAs is claimStoreSlot with the row's kind named: paper reserves
// it as a group drop's paper (reservations.GroupRoomTag, C2c ruling 2), which a
// rank or a committed robot may take; otherwise the plain reserveSlot row. The
// same one INSERT either way. A row the order already holds is kept as it is.
func claimStoreSlotAs(db *store.DB, order *orders.Order, node *nodes.Node, paper bool) error {
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
	reserve := db.ReserveSlot
	if paper {
		reserve = db.ReserveGroupRoom
	}
	if err := reserve(node.ID, order.ID); err != nil {
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
//
// A GROUP DROP'S ROW IS THE GROUP'S PAPER (plainGroupDrop): the slot stands in
// a lane of a group and Core chose it, so the row is tagged GroupRoomTag, and
// that tag is where the plain path keeps the group it chose from until its plan
// is written. The node is read once and its parent once (storageDropoffOf),
// where this read the node three times and the parent once.
func reserveStorageDropoff(db *store.DB, order *orders.Order) error {
	if order.DeliveryNode == "" {
		return nil // no dest — nothing to reserve
	}
	node, err := db.GetNodeByDotName(order.DeliveryNode)
	if err != nil || node == nil {
		// Not readable is not a storage slot here, as isStorageDropoff answered
		// it; the caller's settle-read reports it with the order's id.
		return nil
	}
	storage, lane := storageDropoffOf(db, node)
	if !storage {
		return nil // line / consume point — nothing to reserve
	}
	return claimStoreSlotAs(db, order, node, plainGroupDrop(order, lane))
}

// storageDropoffOf is isStorageDropoff for a node already in hand, and the lane
// the node stands in (nil for a flat child or a top-level node): one read, the
// parent's. A storage dropoff is a STOR node or a child of a LANE or an NGRP,
// exactly isStorageDropoff's union; an unreadable parent answers as
// isConcreteStorageDropoff did, not concrete.
func storageDropoffOf(db *store.DB, node *nodes.Node) (storage bool, lane *nodes.Node) {
	if node == nil || node.IsSynthetic {
		return false, nil
	}
	stor := node.NodeTypeCode == protocol.NodeClassSTOR
	if node.ParentID == nil {
		return stor, nil
	}
	parent, err := db.GetNode(*node.ParentID)
	if err != nil || parent == nil {
		return stor, nil
	}
	switch parent.NodeTypeCode {
	case protocol.NodeClassLANE:
		return true, parent
	case protocol.NodeClassNGRP:
		return true, nil
	}
	return stor, nil
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
	asked := order.DeliveryNode
	if err := d.resolveSyntheticDropoff(order); err != nil {
		return refuse(err)
	}
	resolved := order.DeliveryNode != asked
	d.redirectStoreOffDugLane(order)
	if err := reserveStorageDropoff(d.db, order); err != nil {
		// A SLOT LOST TO A RACE GOES BACK TO ITS GROUP. The resolved slot was
		// written onto the order before the claim, so an order that kept it would
		// wait for that one slot: the winner's bin lands there, and a storage slot
		// can stay full for good while the group has room. Aimed at the group
		// again, the next pass resolves afresh. One update, only on this path.
		//
		// AND SO DOES A GROUP DROP'S PAPER, whenever it was chosen
		// (RULINGS-C2c-run-through §3.2: the plain path keeps the group it chose
		// from). A slot intake settled, or one whose paper a rank took
		// (takeGroupRoomByRank), is a choice of the group's like any other: it
		// goes back to the group rather than waiting on the one slot. Two reads,
		// only on a contended slot.
		if errors.Is(err, ErrSlotContended) {
			back := ""
			if resolved {
				back = asked
			} else if slot, group := d.groupOfPlainDrop(order); group != "" {
				// Its own paper on a slot it cannot have goes with it (a bin landed
				// there), or the row would hold a slot nobody is coming to.
				if rErr := d.db.ReleaseSlotReservation(slot.ID, order.ID); rErr != nil {
					log.Printf("dispatch: order %d could not let go of %s: %v", order.ID, slot.Name, rErr)
				}
				back = group
			}
			if back != "" {
				if uErr := d.db.UpdateOrderDeliveryNode(order.ID, back); uErr != nil {
					log.Printf("dispatch: order %d lost slot %s and could not be aimed back at %s: %v",
						order.ID, order.DeliveryNode, back, uErr)
				} else {
					order.DeliveryNode = back
				}
			}
		}
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

// groupOfPlainDrop is the group a plain order's slot was chosen from, when its
// drop is a group drop (plainGroupDrop), with the slot; or nil and "".
func (d *Dispatcher) groupOfPlainDrop(order *orders.Order) (*nodes.Node, string) {
	node, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil || node == nil {
		return nil, ""
	}
	if _, lane := storageDropoffOf(d.db, node); plainGroupDrop(order, lane) {
		return node, lane.ParentName
	}
	return nil, ""
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
	choice, rErr := d.pure.StoreDrop(node, order.PayloadCode,
		binresolver.UnknownBinType("scanner deferral keeper: order holds no bin and no hold"), digAskerFor(order))
	if rErr == nil && choice.Slot == nil {
		rErr = fmt.Errorf("resolver returned no node")
	}
	if rErr == nil && !d.takeOfferedPaper(order, choice) {
		rErr = fmt.Errorf("no available slot in node group %s", node.Name)
	}
	if rErr != nil {
		return SyntheticUnresolved{OrderID: order.ID, Group: order.DeliveryNode, Err: rErr}
	}
	if uErr := d.db.UpdateOrderDeliveryNode(order.ID, choice.Slot.Name); uErr != nil {
		return fmt.Errorf("order %d resolved %s to %s but delivery_node could not be written: %w",
			order.ID, order.DeliveryNode, choice.Slot.Name, uErr)
	}
	d.dbg("store: order %d resolved group %s -> %s at dispatch", order.ID, order.DeliveryNode, choice.Slot.Name)
	order.DeliveryNode = choice.Slot.Name
	return nil
}

// takeOfferedPaper makes the take the one chooser offered a plain store at
// acquiring (ruling 2, "taken by rank"; the chooser offers a stranger's paper
// only when the group has no free slot and the asker outranks a never-sent
// holder): the holder's row goes, it is told, and the slot is this order's
// paper (takeGroupRoomByRank, re-checked under lock). True when there was no
// take to make or it was made; false when the take was refused, and the store
// then waits as a full group does.
func (d *Dispatcher) takeOfferedPaper(order *orders.Order, choice plan.DropChoice) bool {
	if choice.Take == nil {
		return true
	}
	return d.takeGroupRoomByRank(order, choice.Slot)
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
// The one no-demand order Core DID pick for is a cancel-return
// (recovers_order_id set, engine/cancel_return.go): the policy chose its slot,
// so a dig taking that lane is re-aimed like any other plan, and without this
// the return would hold behind the dig with nothing else to release it.
//
// Best-effort throughout: every doubt leaves the order exactly as it was, and the
// worst case is the behaviour that existed before this function.
func (d *Dispatcher) redirectStoreOffDugLane(order *orders.Order) {
	if d.resolver == nil || order == nil ||
		(order.OriginClass == protocol.OriginClassNoDemand && order.RecoversOrderID == nil) {
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
	choice, err := d.pure.StoreDrop(group, order.PayloadCode,
		binresolver.UnknownBinType("dug-lane redirect: order holds no bin and no hold"), digAskerFor(order))
	if err != nil || choice.Slot == nil || choice.Slot.ID == node.ID {
		d.dbg("store: order %d is aimed at %s in dug lane %s and the group has nowhere else — waiting",
			order.ID, node.Name, lane.Name)
		return
	}
	result := &binresolver.ResolveResult{Node: choice.Slot}
	// A STRANGER'S PAPER THE CHOOSER OFFERED is taken before the old hold goes,
	// take-before-release: a refused take leaves the order aimed where it was.
	if !d.takeOfferedPaper(order, choice) {
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

// The plain path's confirm-at-dispatch (ConfirmForDispatch) is gone: the slot and the bin are confirmed in the order's
// hand now, with its lane mouths, occupancy and status, in one transaction
// (DispatchHand, hand.go). The split it replaced confirmed the slot and the bin in
// separate transactions, so a bin that refused after its slot confirmed left the
// order waiting with the slot held hard.

// confirmDropoffSlot hard-claims a storage slot for an order that is ALREADY
// dispatched: the gate release re-aiming a gate-staged order at a new slot
// (lane_gate_release.go). A pre-dispatch order confirms its slot in its hand
// (acquireHand) instead. Owner-idempotent: a slot this order already claims is
// confirmed in place via ConfirmSlotReservation; otherwise ConfirmSlotClaim runs
// the reservation-guarded hard claim (claim + confirm in one tx, NOT EXISTS bins
// seatbelt).
func (d *Dispatcher) confirmDropoffSlot(order *orders.Order, destNode *nodes.Node) error {
	if destNode.ClaimedBy != nil && *destNode.ClaimedBy == order.ID {
		// Already ours — confirm any still-pending reservation in place, no re-claim.
		return d.db.ConfirmSlotReservation(destNode.ID, order.ID)
	}
	return d.db.ConfirmSlotClaim(destNode.ID, order.ID, nil)
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
