package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/orders"
)

// planBuriedReshuffleAtIntake plans and dispatches a buried-bin reshuffle
// compound for a complex parent that HandleComplexOrderRequest has already
// created, acked and announced. The plan is expose mode
// (PlanReshuffleUnburyOnly): blockers move out of the way, the target bin stays
// where it is, and the parent resumes and re-runs its original first pickup
// against the now-accessible original slot.
//
// THERE IS NO MODE SELECTION HERE ANY MORE. This used to branch on the group's
// reshuffle_target_nodes property into a second planner that relocated the
// target bin to a configured node. That property was never set at either plant
// — Hopkinsville carries no reshuffle_* property at all, Springfield carries
// exactly one and its value is the empty array, which selected THIS path — and
// the dig path had never fired in production at all (0 compound children
// against 2,199 coordinated orders across four months). It was a second planner
// with a second set of downstream discriminations, paid for continuously and
// never once used.
//
// Lane contention: if the buried lane is already locked or TryLock
// races, leave the parent Queued with queue_reason — same disposition
// as planning_service.planBuriedReshuffle.
//
// This used to create the parent itself, building the same 18-field struct as
// complex intake from the same envelope. That made a complex order arriving on
// a buried bin the one complex order whose row was written somewhere else, and
// the two structs had to be kept in step by hand. They are now one struct in
// one place: burial changes what happens after the parent exists, not what the
// parent is. The near-twin handleComplexBuriedOnReplay below stays as it is —
// it is entered from the scanner with an order that already exists, so it never
// had a creation half to fold.
func (d *Dispatcher) planBuriedReshuffleAtIntake(order *orders.Order, payloadCode, stationID string, buried *BuriedError) {
	// Resolve the lane's parent group so the planner has the group ID
	// for the shuffle-slot search.
	// A READ THAT FAILED IS NOT A LANE THAT IS MISSING — see read_vs_missing.go.
	// Releaser for the park: this parent stays `queued`, which is in the acquiring
	// set, so the fulfillment scanner's ordinary retry brings it back through
	// handleComplexBuriedOnReplay.
	lane, err := d.db.GetNode(buried.LaneID)
	if readFailed(err) {
		d.dbg("complex: could not read buried lane %d for parent %d (%v) — holding", buried.LaneID, order.ID, err)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseReadFailed,
			QueueParams{Payload: payloadCode})
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}
	if err != nil || lane == nil {
		d.failOrderInternal(order, codeInvalidNode, configFailureID("lane node", buried.LaneID))
		return
	}
	if lane.ParentID == nil {
		d.failOrderInternal(order, codeInvalidNode, fmt.Sprintf(
			"config failure: lane %s is not in a node group, so it has nowhere to park a blocker", lane.Name))
		return
	}
	groupID := *lane.ParentID

	// The parent is queued because its bin is buried, so say so now, before any
	// of the dispositions below. It used to be recorded only when one of the
	// three contention arms fired — so the ORDINARY burial, the one where the
	// lane is free and the reshuffle dispatches, was the case that recorded
	// nothing. That blank does not stay on the row: historyReason copies
	// QueueCode into the history entry for any transition into queued, and the
	// parent's first such transition is the reshuffle completing and resuming
	// it. By then the row can be corrected and the history cannot.
	//
	// The arms below refine the cause. They keep the same sentence and code —
	// storage is being rearranged either way, which is what the operator needs
	// to know — and differ in the engineer-only tag for where the wait arose.
	d.setQueueReason(order, protocol.QueueStorageRearranging, CauseIntakeBuried,
		QueueParams{Lane: lane.Name, Payload: payloadCode})

	// Lane-contention: leave the parent Queued for scanner replay.
	// Asks "may I CLAIM this lane for a dig", not "may this move happen now" —
	// see planning_service.go planBuriedReshuffle for why delegating to admission
	// would refuse every reshuffle plan by construction.
	if d.laneLock.IsLocked(buried.LaneID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, CauseLaneLocked,
			QueueParams{Lane: lane.Name, Payload: payloadCode})
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}

	plan, err := PlanReshuffleUnburyOnly(d.db, buried.Bin, buried.Slot, lane, groupID)
	// NO FREE SHUFFLE SLOT IS CONGESTION HERE TOO. planBuriedReshuffle grew this
	// arm when sim order 21 died of it on 2026-07-10; the two complex sites are
	// the same call with the same error and never got it, so the identical
	// congestion terminated a complex parent and waited for a plain one.
	//
	// Surfaced again on the lane-stress rig 2026-08-09, and it will keep
	// surfacing: tightening the shuffle pool (a gated dig may not park in another
	// gated lane) makes "no slot right now" a routine outcome rather than an
	// exotic one. That tightening is only safe because this outcome waits.
	//
	// Everything else out of the planner is real lane geometry and stays terminal
	// — the same split the simple path draws, drawn the same way.
	//
	// RELEASER FOR THE PARK: the parent is still `queued` — the plan failed before
	// CreateCompoundOrder, so nothing moved it — and `queued` is in the acquiring
	// set, so the ordinary scanner replay brings it back through this function
	// against a group that by then has room. The same releaser the lane-locked arm
	// above already rests on, which is why no new subscription is needed.
	if errors.Is(err, ErrNoShuffleSlot) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, CauseNoShuffleSlot,
			QueueParams{Lane: lane.Name, Payload: payloadCode})
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}
	if err != nil {
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot plan reshuffle: %v", err))
		return
	}

	// Race-safe lock acquisition.
	if !d.laneLock.TryLock(buried.LaneID, order.ID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, CauseLaneLockRace,
			QueueParams{Lane: lane.Name, Payload: payloadCode})
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}

	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID, order.ID)
		// Congestion, not a fault: see planning_service.go planBuriedReshuffle for
		// why a blocker held outside the compound waits. The parent is still
		// `queued` (CreateCompoundOrder writes the children before it moves the
		// parent), so the scanner's replay picks it up and this is a park, not a
		// stall.
		if errors.Is(err, store.ErrBlockerClaimed) {
			d.setQueueReason(order, protocol.QueueStorageRearranging, CauseDigBlockerClaimed,
				QueueParams{Lane: lane.Name, Payload: payloadCode})
			d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
			return
		}
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot create compound order: %v", err))
		return
	}
	// Persist the lane-extension entry NOW so the listener at
	// AdvanceCompoundOrder terminal can look up the target bin ID directly
	// instead of re-deriving from lane state. Unconditional since target-node
	// mode went: every complex dig is an expose, and every expose holds its lane
	// past completion.
	{
		if _, err := d.db.InsertPendingLaneExtension(&store.PendingLaneExtension{
			ComplexParentID:    order.ID,
			LaneID:             buried.LaneID,
			TargetBinID:        buried.Bin.ID,
			ExpectedFromNodeID: buried.Slot.ID,
		}); err != nil {
			log.Printf("dispatch: persist pending_lane_extension at intake for complex %d: %v", order.ID, err)
			// Non-fatal: the at-terminal arming path will still
			// run; if the row is missing then, it falls back to
			// the unconditional unlock. Loss is crash resilience
			// only.
		}
	}
	d.dbg("complex: compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))

}

// handleComplexBuriedOnReplay handles a burial discovered by the
// scanner-path re-resolve (after the parent has resumed from a prior
// reshuffle). Pivots the parent Queued → Reshuffling and dispatches a
// fresh compound. Same dual-mode logic as the intake path but without
// the parent-creation step — the order already exists.
//
// Multi-burial loop: each successful resume → re-resolve cycle that
// discovers a new burial gets its own compound. v6's livelock cap was
// removed in v7 — the lane-lock extension closes the only realistic
// re-burial vector for expose mode, and sequential legitimate burials
// in a multi-pickup complex order shouldn't be punished with a
// terminal fail.
func (d *Dispatcher) handleComplexBuriedOnReplay(order *orders.Order, buried *BuriedError) {
	// Same three-way split as the intake twin. This path is entered from the
	// scanner with the parent already acquiring, so leaving it queued with a cause
	// IS the retry — the releaser is the scan that brought us here.
	lane, err := d.db.GetNode(buried.LaneID)
	if readFailed(err) {
		d.dbg("complex: could not read buried lane %d for parent %d (%v) — holding", buried.LaneID, order.ID, err)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseReadFailed,
			QueueParams{Payload: order.PayloadCode})
		return
	}
	if err != nil || lane == nil {
		d.failOrderInternal(order, codeInvalidNode, configFailureID("lane node", buried.LaneID))
		return
	}
	if lane.ParentID == nil {
		d.failOrderInternal(order, codeInvalidNode, fmt.Sprintf(
			"config failure: lane %s is not in a node group, so it has nowhere to park a blocker", lane.Name))
		return
	}
	groupID := *lane.ParentID

	// Asks "may I CLAIM this lane for a dig", not "may this move happen now" —
	// see planning_service.go planBuriedReshuffle for why delegating to admission
	// would refuse every reshuffle plan by construction.
	if d.laneLock.IsLocked(buried.LaneID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, CauseLaneLocked,
			QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
		return
	}

	plan, err := PlanReshuffleUnburyOnly(d.db, buried.Bin, buried.Slot, lane, groupID)
	// Congestion, not geometry — see the sibling site in planBuriedReshuffleAtIntake.
	//
	// No emit here, matching the lane-locked arm above: this path is entered from
	// the scanner with the parent already acquiring, so leaving it queued with a
	// cause IS the retry.
	if errors.Is(err, ErrNoShuffleSlot) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, CauseNoShuffleSlot,
			QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
		return
	}
	if err != nil {
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot plan reshuffle: %v", err))
		return
	}

	if !d.laneLock.TryLock(buried.LaneID, order.ID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, CauseLaneLockRace,
			QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
		return
	}
	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID, order.ID)
		// Same congestion arm as the intake twin above. This path is entered from
		// the scanner with the parent already `queued`, so leaving it queued with a
		// cause IS the retry — the next scan re-plans against a lane the holder has
		// by then left.
		if errors.Is(err, store.ErrBlockerClaimed) {
			d.setQueueReason(order, protocol.QueueStorageRearranging, CauseDigBlockerClaimed,
				QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
			return
		}
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot create compound order on replay: %v", err))
		return
	}
	// Same persistence as the intake path. See the comment in
	// planBuriedReshuffleAtIntake.
	{
		if _, err := d.db.InsertPendingLaneExtension(&store.PendingLaneExtension{
			ComplexParentID:    order.ID,
			LaneID:             buried.LaneID,
			TargetBinID:        buried.Bin.ID,
			ExpectedFromNodeID: buried.Slot.ID,
		}); err != nil {
			log.Printf("dispatch: persist pending_lane_extension at replay for complex %d: %v", order.ID, err)
		}
	}
	d.dbg("complex: replay compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))

}
