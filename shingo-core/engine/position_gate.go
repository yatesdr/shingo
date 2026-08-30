package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/fleet/seerrds"
	"shingocore/store/bins"
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
//
// A LOOKUP FAILURE NEVER BLOCKS — with ONE exception, and the exception is stated
// here because it contradicts the sentence before it. When the position is
// occupied and the ORDER cannot be resolved from its vendor id, the gate holds.
// It reads the other way round from every other failure in this function, and it
// is right: at that point occupancy is a FACT already read, and the only unknown
// is whether the resident belongs to the arriving order. Passing would let a
// robot lower a bin onto an occupied node, which is the impossible event this
// gate exists to keep out of Core's model. Every other read failure leaves
// occupancy itself unknown, and inventing a stall out of a missing row is the
// worse error.
//
// The arm's population is the vendor order — an order the fleet knows and Core
// cannot look up. The simulator does not produce one, so no fixture reaches it;
// it is pinned by a unit test rather than by a run.
//
// THE OCCUPANCY QUESTION IS SPLIT OUT (positionOccupiedBy) so a reader that wants
// "is this position obstructed" gets exactly that, without the gate's fail-closed
// arm riding along. A second spelling of the occupancy rule is how the simulator's
// physics and anything that later reports on it drift apart.
func (e *Engine) CanEnterPosition(vendorOrderID, location, binTask string) (bool, string) {
	// Only a placement can be obstructed. Anything else — pickup, wait, or a task
	// we do not recognise — passes untouched.
	if binTask != seerrds.BinTaskForAction(protocol.ActionDropoff) {
		return true, ""
	}

	blocker, occupied := e.positionOccupiedBy(location, 0)
	if !occupied {
		return true, ""
	}

	order, err := e.db.GetOrderByVendorID(vendorOrderID)
	if err != nil || order == nil {
		return false, fmt.Sprintf("%s holds bin %d and the order is unresolvable", location, blocker.ID)
	}

	// Re-ask with the order's identity: a bin this order already owns is not an
	// obstruction to itself (a multi-bin order placing beside its own load).
	blocker, occupied = e.positionOccupiedBy(location, order.ID)
	if !occupied {
		return true, ""
	}
	return false, fmt.Sprintf("%s holds bin %d (claimed by %s), order %d cannot place onto it",
		location, blocker.ID, claimOwner(blocker.ClaimedBy), order.ID)
}

// positionOccupiedBy is the occupancy half of CanEnterPosition, on its own so
// there is ONE spelling of "this position is physically obstructed".
//
// forOrderID, when non-zero, excludes bins that order already owns — the
// multi-bin case where a leg places beside its own load. Zero means "obstructed
// by anything", which is the question to ask when the order is not known.
//
// ── IT FAILS OPEN, ALWAYS, AND THAT IS NOT THE GATE'S WHOLE ANSWER ────────
//
// Every read failure here reports NOT occupied. This is a physics model, not a
// validator: a missing node row or an unreadable bin list must not invent a
// stall. CanEnterPosition's fail-CLOSED arm lives at the call site above,
// deliberately outside this function, because it fires on a DIFFERENT unknown —
// occupancy is already established there and only ownership is in doubt. A
// reader that wants occupancy gets occupancy; a reader that wants the gate's
// full decision calls the gate.
//
// Retired bins are not obstructions — the row survives for audit, the carrier is
// gone from the floor — and there is no arm here for them because there does not
// need to be: bins.ListByNode's own WHERE carries `b.status != 'retired'`, so
// they never arrive. There WAS one, skipping a status the query had already
// excluded, and the exclusion is stated here rather than re-implemented so a
// reader is not left thinking the guard is load-bearing.
func (e *Engine) positionOccupiedBy(location string, forOrderID int64) (*bins.Bin, bool) {
	node, err := e.db.GetNodeByDotName(location)
	if err != nil || node == nil || node.IsSynthetic {
		return nil, false
	}
	residents, err := e.db.ListBinsByNode(node.ID)
	if err != nil {
		return nil, false
	}
	for _, b := range residents {
		if forOrderID != 0 && b.ClaimedBy != nil && *b.ClaimedBy == forOrderID {
			continue
		}
		return b, true
	}
	return nil, false
}

func claimOwner(claimedBy *int64) string {
	if claimedBy == nil {
		return "nobody"
	}
	return fmt.Sprintf("order %d", *claimedBy)
}

// Compile-time check: the engine satisfies the gate the simulator asks for.
var _ fleet.PositionGate = (*Engine)(nil)
