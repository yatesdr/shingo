package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/fleet/seerrds"
)

// CanEnterPosition implements fleet.PositionGate: the plant's one-bin-per-node
// invariant, taught to the simulator.
//
// A plant node holds exactly ONE bin. A robot physically cannot lower a bin onto a
// position that already holds one, so in the field the block simply never reports
// FINISHED — the robot stalls until the position clears. The real fleet gets that
// from physics and needs no gate. The simulator has no physics: it completes every
// block on a timer, so it must ask.
//
// Why this matters, concretely (chased 2026-07-13): in a two-robot press swap the
// timer-only driver "delivered" the empty onto the press BEFORE the other robot had
// lifted the full bin out. That cannot happen in a plant. Core, handed an impossible
// event, did the only correct thing with it — a completed delivery proves the slot
// was empty, so the bin still recorded there must be a stale ghost — and evicted a
// perfectly good bin. Core was right; the sim was lying.
//
// ONLY A PLACEMENT CAN BE BLOCKED. A pickup is the robot REMOVING the bin that is
// there; a wait is it standing next to one. Neither can be obstructed by occupancy,
// and holding them deadlocks the robot against the very bin it came for.
//
// This is not theoretical: an earlier version keyed off ownership alone, reasoning
// that the bin at a pickup is always the order's own. It is not. ApplyArrival CLEARS
// a bin's claim when it lands, so a compound restock leg arrives to collect a bin
// that is claimed by nobody — and the gate held two robots at their own shuffle
// slots for six minutes until the sim surfaced it. Key off the block's task.
//
//   - JackLoad (pickup) / Wait → never held.
//   - JackUnload (placement) onto a free node → pass.
//   - JackUnload onto another bin → HOLD. That is the press swap: the empty-in waits
//     for the full-out to lift its bin clear, which is the real choreography.
//
// Synthetic nodes (LANE / NGRP / _TRANSIT) hold many bins by design and are exempt.
// A lookup failure never blocks: this is a physics model, not a validator, and it
// must not invent a stall out of a missing row.
func (e *Engine) CanEnterPosition(vendorOrderID, location, binTask string) (bool, string) {
	// Only a placement can be obstructed. Anything else — pickup, wait, or a task
	// we do not recognise — passes untouched.
	if binTask != seerrds.BinTaskForAction(protocol.ActionDropoff) {
		return true, ""
	}

	node, err := e.db.GetNodeByDotName(location)
	if err != nil || node == nil || node.IsSynthetic {
		return true, ""
	}

	residents, err := e.db.ListBinsByNode(node.ID)
	if err != nil || len(residents) == 0 {
		return true, "" // position is free
	}

	order, err := e.db.GetOrderByVendorID(vendorOrderID)
	if err != nil || order == nil {
		return false, fmt.Sprintf("%s holds bin %d and the order is unresolvable", location, residents[0].ID)
	}

	for _, b := range residents {
		if b.Status == "retired" {
			continue
		}
		// A bin this order already owns is not an obstruction to itself (a multi-bin
		// order placing beside its own load).
		if b.ClaimedBy != nil && *b.ClaimedBy == order.ID {
			continue
		}
		return false, fmt.Sprintf("%s holds bin %d (claimed by %s), order %d cannot place onto it",
			location, b.ID, claimOwner(b.ClaimedBy), order.ID)
	}
	return true, ""
}

func claimOwner(claimedBy *int64) string {
	if claimedBy == nil {
		return "nobody"
	}
	return fmt.Sprintf("order %d", *claimedBy)
}

// Compile-time check: the engine satisfies the gate the simulator asks for.
var _ fleet.PositionGate = (*Engine)(nil)
