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
	case LaneEnforceDelegated:
		return LaneEnforceDelegated
	default:
		return LaneEnforceNone
	}
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
	if d.laneEnforcementMode(*lane.ParentID) != LaneEnforceMouth {
		return 0, false // group not mouth-enforced → byte-identical
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
		if d.laneEnforcementMode(*lane.ParentID) != LaneEnforceMouth {
			return nil // group not mouth-enforced
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
	return reservations.ReleaseLane(d.db.DB, orderID, lane.ID)
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
