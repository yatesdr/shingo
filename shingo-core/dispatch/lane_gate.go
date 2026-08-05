package dispatch

import (
	"errors"
	"log"

	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// LaneEnforcementMode is the per-lane-group enforcement choice — the §15 methods
// menu, picked per plant topology and stored as a node property on the group
// (NGRP). Default is none: the Core mouth gate is off and behavior is identical
// to today.
type LaneEnforcementMode string

const (
	// LaneEnforceNone — no Core lane gate for this group (the default). Byte-
	// identical to pre-seam behavior.
	LaneEnforceNone LaneEnforcementMode = "none"
	// LaneEnforceMouth — the mode-aware mouth gate (§2): shared storage lanes that
	// need same-kind concurrency; a conflicting order waits, visibly, in sourcing.
	LaneEnforceMouth LaneEnforcementMode = "mouth"
	// LaneEnforceDelegated — an RDS mutex zone owns the physical mouth (B4); Core
	// takes no mouth hold for this group.
	LaneEnforceDelegated LaneEnforcementMode = "delegated"
	// LaneEnforceGateChoreography — Core is the traffic cop: a lane-bound order
	// ships UNSEALED ending at the lane's wait point, and Core appends its tail
	// when the lane is safe. Same substrate as mouth (mouth rows, §4 release,
	// depth priority) and the SAME classifier; only the disposition of a park
	// verdict differs — mouth parks the order pre-dispatch, choreography stages
	// the robot at the gate. Introduced inert: until the staging path lands, a
	// group set to this value behaves exactly like `mouth`.
	LaneEnforceGateChoreography LaneEnforcementMode = "gate_choreography"
)

// PropLaneEnforcement is the node-property key (read on the lane's group) that
// selects the enforcement mode.
const PropLaneEnforcement = "lane_enforcement"

// laneGateReservedBy tags the mouth rows the gate writes, for forensics.
const laneGateReservedBy = "lanegate"

// laneShareBasePriority is the RDS priority floor for a robot entering a
// mouth-enforced lane; the relevant slot's depth is added on top. RDS serves the
// LARGEST priority first — the vendor manual is explicit: "a larger number
// indicates a higher order priority" (reference/RDSCore HTTP API_Aivison_20260430.pdf,
// setOrder priority field; the RDS team's conventional "priority": 10 is a boost) —
// so a deeper store target gets a LARGER value and is dispatched into the
// single-file lane ahead of the shallower stores stacking in behind it.
//
// Honest note on the base (owner-approved): under larger-wins, any positive base
// makes a lane entry outrank default-priority (0) work — and also the conventional
// priority-10 boosts. That is intended, NOT a yield: these orders were already
// admitted and dispatched; the value only SEQUENCES lane entries by depth, and it
// does so ahead of unprioritized work. ◆ open: if an operator boost should beat
// routine lane sequencing, drop the base below 10 (still > 0) — flagged for owner.
const laneShareBasePriority = 30

// laneEnforcementMode reads the enforcement mode configured on a lane group
// (NGRP). Any unset or unrecognized value is none — off, byte-identical to today.
func (d *Dispatcher) laneEnforcementMode(groupID int64) LaneEnforcementMode {
	switch LaneEnforcementMode(d.db.GetNodeProperty(groupID, PropLaneEnforcement)) {
	case LaneEnforceMouth:
		return LaneEnforceMouth
	case LaneEnforceGateChoreography:
		return LaneEnforceGateChoreography
	case LaneEnforceDelegated:
		return LaneEnforceDelegated
	default:
		return LaneEnforceNone
	}
}

// laneGateActive reports whether CORE owns this group's lane mouth — i.e.
// whether the Core-side lane machinery (mouth holds, the depth-sequenced RDS
// priority, the tiered-entry classifier) applies at all.
//
// It exists to make adding an arm safe. Every one of those three call sites used
// to spell the test `!= LaneEnforceMouth`, which silently means "any mode that is
// not literally mouth gets NO gate" — so a new arm would fall through to the
// `none` behavior and quietly ship with no mouth holds, no depth priority, and no
// classifier, on a plant that had explicitly configured a gate. That failure is
// invisible: nothing errors, orders just stop being sequenced. Routing the tests
// through one predicate is what keeps the next arm from inheriting that.
//
// `delegated` is deliberately NOT active: an RDS mutex zone owns that mouth, so
// Core takes no hold — that is the whole meaning of the value. `none` is off.
//
// What an ACTIVE mode still chooses for itself is the DISPOSITION of a classifier
// park verdict (mouth: park pre-dispatch; gate_choreography: stage at the wait
// point). That branch belongs in admitLaneEntry, never here.
func laneGateActive(m LaneEnforcementMode) bool {
	return m == LaneEnforceMouth || m == LaneEnforceGateChoreography
}

// laneDispatchPriority returns the depth-sequenced RDS priority for a robot
// entering a mouth-enforced lane at its DROPOFF (destNode) — the inbound leg — and
// ok=false otherwise (so the caller keeps order.Priority; byte-identical when no
// group enforces the mouth). Larger wins at RDS, so a deeper target slot gets a
// LARGER value: priority = base + targetSlotDepth. Stateless — a pure function of
// the slot's depth — so co-admitted stores sequence back-to-front with no per-lane
// counter and no reset (a fresh store into an emptied lane resolves to the deepest
// slot again → the largest value again).
//
// This is the destination-lane (inbound) leg. Per the owner scope ruling, lane-
// entry priority covers ALL lane entries: the source-lane (outbound, shallowest-
// source-first) mirror, two-lane orders (a value per leg), and complex/coordinated
// orders are SEQUENCED FOLLOW-UPS — not permanent gaps (see the phase log).
func (d *Dispatcher) laneDispatchPriority(destNode *nodes.Node) (int, bool) {
	if destNode == nil {
		return 0, false
	}
	lane, err := d.db.LaneForNode(destNode.ID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return 0, false // not a lane slot, or a lane with no group
	}
	if !laneGateActive(d.laneEnforcementMode(*lane.ParentID)) {
		return 0, false // Core does not own this group's mouth → byte-identical
	}
	targetDepth, err := d.db.GetSlotDepth(destNode.ID)
	if err != nil {
		return 0, false
	}
	return laneShareBasePriority + targetDepth, true
}

// laneHold is a (lane, mode) an order must hold to work that lane: outbound when
// it picks from the lane, inbound when it drops into it.
type laneHold struct {
	laneID int64
	mode   reservations.Mode
}

// resolveOrderLaneHolds computes the mouth holds a plain order needs from its
// source and destination nodes: the source lane is outbound (the order picks
// there), the destination lane is inbound (the order drops there). A node that is
// not a direct lane slot (LaneForNode == nil) contributes no hold, and only lanes
// whose group is configured for mouth enforcement are included — none/delegated
// groups contribute nothing, so an unconfigured plant yields zero holds (the gate
// is a no-op).
func (d *Dispatcher) resolveOrderLaneHolds(sourceNode, destNode *nodes.Node) ([]laneHold, error) {
	var holds []laneHold
	add := func(n *nodes.Node, mode reservations.Mode) error {
		if n == nil {
			return nil
		}
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			return err
		}
		if lane == nil || lane.ParentID == nil {
			return nil // not a lane slot, or a lane with no group — no hold
		}
		if !laneGateActive(d.laneEnforcementMode(*lane.ParentID)) {
			return nil // Core does not own this group's mouth
		}
		holds = append(holds, laneHold{laneID: lane.ID, mode: mode})
		return nil
	}
	if err := add(sourceNode, reservations.ModeOutbound); err != nil {
		return nil, err
	}
	if err := add(destNode, reservations.ModeInbound); err != nil {
		return nil, err
	}
	return holds, nil
}

// acquireOrderLanes takes every hold the order needs, all-or-nothing across
// modes. It returns admitted=false on a mode conflict (the caller requeues the
// order in sourcing under WAITING_FOR_SLOT, per Rule 1); a non-nil error is a
// transient DB failure. With no holds (the common, unconfigured case) it admits
// immediately.
//
// AcquireLanes is per-mode all-or-nothing; an order picking from one lane and
// dropping into another needs one call per mode, so a conflict on the second
// mode rolls back the first via the order-scoped ReleaseLanesByOwner. All rows
// are owned by the order, so that release reclaims exactly this gate's takes.
func (d *Dispatcher) acquireOrderLanes(orderID int64, holds []laneHold) (admitted bool, err error) {
	if len(holds) == 0 {
		return true, nil
	}
	byMode := map[reservations.Mode][]int64{}
	for _, h := range holds {
		byMode[h.mode] = append(byMode[h.mode], h.laneID)
	}
	for mode, lanes := range byMode {
		if aErr := reservations.AcquireLanes(d.db.DB, orderID, mode, laneGateReservedBy, lanes...); aErr != nil {
			if errors.Is(aErr, reservations.ErrReservationConflict) {
				// Roll back any holds taken for an earlier mode so the acquire is
				// all-or-nothing across the order's lanes.
				_ = reservations.ReleaseLanesByOwner(d.db.DB, orderID)
				return false, nil
			}
			return false, aErr
		}
	}
	return true, nil
}

// releaseOrderLaneFor releases the order's mouth hold on the lane that contains
// node, if any — the early per-block handoff (§4): an outbound hold drops when
// the bin leaves the lane, an inbound hold drops when the bin lands. Owner-scoped
// and a no-op when no row exists, so it is byte-identical when the gate is off.
//
// A DIG CLAIM IS EXEMPT. This is a per-VISIT release and a dig's hold is
// per-RESHUFFLE: it has to outlive several legs, and the first leg's pickup is
// not the end of anything. Because laneOwnerFor routes a child's block progress
// to the compound parent, this used to arrive owned by the parent, match the
// parent's dig row, and delete it at leg one — leaving every later leg of the
// reshuffle with no durable claim on the lane at all.
//
// The exemption lives in ReleaseLaneHandoff's SQL rather than here, so there is
// no window between reading the mode and deleting the row. Everything else about
// the handoff is unchanged, which is the point: it is right for plain orders and
// was wrong only for digs.
func (d *Dispatcher) releaseOrderLaneFor(orderID int64, node *nodes.Node) error {
	if node == nil {
		return nil
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil {
		return err
	}
	if lane == nil {
		return nil
	}
	return reservations.ReleaseLaneHandoff(d.db.DB, orderID, lane.ID)
}

// TakeLaneOccupancy records that an order is INSIDE the lanes it was just
// dispatched into — Hold B, the second of the two lane holds.
//
// Called at dispatch rather than on any robot signal, and that is the design
// rather than a shortcut. Core cannot observe a robot entering a lane: every
// available signal is lagging, the earliest being a block completing AT a slot,
// which is already inside. It does not need to. Nothing enters a lane that Core
// did not send there, so Core's own dispatch decision IS the entry moment, and
// it is knowable exactly when it becomes true.
//
// Both endpoints are considered: a dig leg picks OUT of one lane and may place
// INTO another, and the robot is inside each while it is there. A node that is
// not a lane slot contributes nothing.
//
// Advisory today. It records; it does not arbitrate. While the compound
// scheduler still dispatches one child at a time, at-most-one-inside is already
// guaranteed by that, and this exists so the fact is durable and observable
// before anything is permitted to rely on it.
func (d *Dispatcher) TakeLaneOccupancy(orderID int64, nodes ...*nodes.Node) {
	for _, laneID := range d.lanesFor(nodes...) {
		if err := reservations.AcquireOccupancy(d.db.DB, orderID, laneID); err != nil {
			log.Printf("lanegate: take occupancy for order %d on lane %d: %v", orderID, laneID, err)
		}
	}
}

// ReleaseLaneOccupancy records that an order is out of every lane it occupied.
//
// Fired on DROPOFF completion, not on pickup. After a pickup the robot is
// holding the bin and still physically in the lane; it is out once it has placed
// at the destination. Releasing at pickup would declare the lane free with a
// robot still standing in it, which is the whole failure this hold exists to
// prevent.
//
// It releases ALL of the order's occupancy rather than the drop node's lane,
// because a dig leg's two endpoints are usually different lanes: it is the
// SOURCE lane the robot has finally left by placing elsewhere. "This leg has
// placed its bin and is out" is a statement about the leg, not about one node.
//
// Terminalization needs no separate arm: TerminalizeOrder's ReleaseByOrder is
// order-keyed and kind-agnostic, so a child that fails or is cancelled drops its
// occupancy in the same transaction that ends it.
func (d *Dispatcher) ReleaseLaneOccupancy(orderID int64) {
	if err := reservations.ReleaseAllOccupancy(d.db.DB, orderID); err != nil {
		log.Printf("lanegate: release occupancy for order %d: %v", orderID, err)
	}
}

// lanesFor maps nodes to the distinct lanes that contain them, skipping nodes
// that are not lane slots.
func (d *Dispatcher) lanesFor(ns ...*nodes.Node) []int64 {
	seen := make(map[int64]bool, len(ns))
	var out []int64
	for _, n := range ns {
		if n == nil {
			continue
		}
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			log.Printf("lanegate: resolve lane for node %d: %v", n.ID, err)
			continue
		}
		if lane == nil || seen[lane.ID] {
			continue
		}
		seen[lane.ID] = true
		out = append(out, lane.ID)
	}
	return out
}

// causeForLaneHolds classifies a lane conflict for the operator-facing queue
// reason (§6): lane-held-dig if any of the order's target lanes is held by a dig,
// otherwise lane-held-traffic (a different-mode collision). Engineer-only detail;
// the operator sees the same "Waiting for a slot at ‹lane›" either way.
func (d *Dispatcher) causeForLaneHolds(orderID int64, holds []laneHold) string {
	for _, h := range holds {
		rows, err := reservations.ActiveMouthRows(d.db.DB, h.laneID)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.OrderID != orderID && r.Mode == reservations.ModeDig {
				return "lane-held-dig"
			}
		}
	}
	return "lane-held-traffic"
}

// AcquireLanesForOrder takes the mouth holds a plain order needs before it
// dispatches — outbound on its source lane, inbound on its destination lane, for
// lanes whose group is configured for mouth enforcement. The scanner calls it
// just before the fleet commit; on a conflict it returns admitted=false plus the
// operator cause and the contended lane's name, and the scanner parks the order
// in sourcing under WAITING_FOR_SLOT holding its soft reservations (Rule 1).
//
// admitted=true with empty cause/lane means there was nothing to gate (no
// mouth-enforced lane on the order's path), so an unconfigured plant is a no-op
// and behavior is byte-identical. A non-nil error is a transient DB failure.
func (d *Dispatcher) AcquireLanesForOrder(orderID int64, sourceNode, destNode *nodes.Node) (admitted bool, cause, laneName string, err error) {
	holds, err := d.resolveOrderLaneHolds(sourceNode, destNode)
	if err != nil {
		return false, "", "", err
	}
	if len(holds) == 0 {
		return true, "", "", nil // nothing gated — byte-identical to today
	}
	admitted, err = d.acquireOrderLanes(orderID, holds)
	if err != nil {
		return false, "", "", err
	}
	if admitted {
		return true, "", "", nil
	}
	return false, d.causeForLaneHolds(orderID, holds), d.laneDisplayName(holds), nil
}

// laneDisplayName returns a human name for the first contended lane, for the
// "Waiting for a slot at ‹lane›" queue sentence.
func (d *Dispatcher) laneDisplayName(holds []laneHold) string {
	if len(holds) == 0 {
		return ""
	}
	if n, err := d.db.GetNode(holds[0].laneID); err == nil && n != nil {
		return n.Name
	}
	return ""
}

// ReleaseLanesForOrder drops all of an order's mouth holds. Used on a fleet-
// dispatch failure rollback: the robot never committed, so the hold taken by
// AcquireLanesForOrder must not linger and block the lane. Owner-scoped; a no-op
// when the order holds none.
func (d *Dispatcher) ReleaseLanesForOrder(orderID int64) error {
	return reservations.ReleaseLanesByOwner(d.db.DB, orderID)
}

// laneOwnerFor resolves the order that OWNS a lane mouth row for a block: the
// order itself for a plain order, or its complex parent for a compound child
// (children never own rows, §2). So a child's block progress releases the
// parent-owned hold.
func (d *Dispatcher) laneOwnerFor(orderID int64) int64 {
	o, err := d.db.GetOrder(orderID)
	if err != nil || o == nil || o.ParentOrderID == nil {
		return orderID
	}
	return *o.ParentOrderID
}

// HandleTransitForLaneGate releases the owner's mouth hold on the lane a picked
// bin just LEFT (§4 pickup / early handoff). Fired on EventBinEnteredTransit,
// routed to the compound parent for a child. A no-op when the from-node is not a
// lane or no mouth row is held — byte-identical when the gate is off.
func (d *Dispatcher) HandleTransitForLaneGate(orderID, fromNodeID int64) {
	if fromNodeID == 0 {
		return
	}
	owner := d.laneOwnerFor(orderID)
	node, err := d.db.GetNode(fromNodeID)
	if err != nil || node == nil {
		return
	}
	if err := d.releaseOrderLaneFor(owner, node); err != nil {
		log.Printf("lanegate: release hold for order %d (owner %d) on transit from node %d: %v",
			orderID, owner, fromNodeID, err)
	}
}

// ReleaseInboundLaneForOrder releases the owner's mouth hold on the lane an order
// just DROPPED into (§4 dropoff / early handoff). Fired from the store block-
// completion handler BEFORE the delivery-node early-return, routed to the compound
// parent for a child. A no-op when the drop node is not a lane or no mouth row is
// held.
func (d *Dispatcher) ReleaseInboundLaneForOrder(orderID int64, dropNodeName string) {
	if dropNodeName == "" {
		return
	}
	owner := d.laneOwnerFor(orderID)
	node, err := d.db.GetNodeByDotName(dropNodeName)
	if err != nil || node == nil {
		return
	}
	if err := d.releaseOrderLaneFor(owner, node); err != nil {
		log.Printf("lanegate: release hold for order %d (owner %d) on dropoff at %s: %v",
			orderID, owner, dropNodeName, err)
	}
}
