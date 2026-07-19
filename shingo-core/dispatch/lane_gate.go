package dispatch

import (
	"errors"

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
