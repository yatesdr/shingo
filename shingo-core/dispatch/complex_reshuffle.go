package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// handleComplexBuriedAtIntake creates the complex parent at Queued,
// acks edge, then plans and dispatches a buried-bin reshuffle
// compound. Branches on the source group's reshuffle_target_nodes
// property:
//
//   - empty → expose mode (PlanReshuffleUnburyOnly). Parent resumes
//     and re-runs its original first pickup against the now-
//     accessible original slot.
//   - non-empty with at least one empty target → target-node mode
//     (PlanReshuffleToTarget). Compound moves the target bin to the
//     first empty configured target; parent re-resolves against the
//     group on resume and finds it at the target node.
//   - non-empty with all targets occupied → leave parent Queued with
//     queue_reason. Scanner replays on bin/order events; once a
//     target frees the next replay proceeds.
//
// Lane contention: if the buried lane is already locked or TryLock
// races, leave the parent Queued with queue_reason — same disposition
// as planning_service.planBuriedReshuffle.
func (d *Dispatcher) handleComplexBuriedAtIntake(env *protocol.Envelope, p *protocol.ComplexOrderRequest, payloadCode string, buried *BuriedError) {
	stationID := env.Src.Station
	d.dbg("complex: order %s buried at intake — bin %d in lane %d (slot %s)",
		p.OrderUUID, buried.Bin.ID, buried.LaneID, buried.Slot.Name)

	// Preserve the original NGRP-bearing step shape so the resume path
	// (parent → Queued → scanner → reResolveComplexSteps) has the input
	// it needs to re-resolve once the compound completes.
	resolvedSteps := stepsAsResolved(p.Steps)
	stepsJSON, err := json.Marshal(resolvedSteps)
	if err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to marshal steps")
		return
	}
	sourceNode, deliveryNode := extractEndpoints(resolvedSteps)

	// INTAKE SITE 3 OF 3, AND IT IS INTAKE — NOT A DERIVATIVE.
	//
	// This function is reached from HandleComplexOrderRequest's ResolutionBuried
	// branch, which RETURNS rather than falling through, so the order built here
	// is the complex parent itself and its origin comes off the same envelope
	// (p.OriginID) as the main path's. The reshuffle compound created below IS
	// derivative and inherits from this row via CreateCompoundOrder.
	//
	// Miss this site and every complex order that happens to arrive on a buried
	// bin becomes an orphan — a bucket that fills in proportion to how full the
	// lanes are, i.e. worst exactly when the surface matters most.
	originID, originClass := classifyInboundOrigin(p.OriginID, p.OriginClass, stationID, p.OrderUUID)

	order := &orders.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     p.Quantity,
		Priority:     p.Priority,
		PayloadCode:  payloadCode,
		PayloadDesc:  p.PayloadDesc,
		SourceNode:   sourceNode,
		DeliveryNode: deliveryNode,
		ProcessNode:  p.ProcessNode,
		StepsJSON:    string(stepsJSON),
		// Provenance stamp: complex intake (buried path) is coordinated.
		Coordinated: true,
		// Persist the swap sibling on the buried path too (durable forward
		// link) — same contract as the main intake path above.
		SiblingOrderUUID: p.SiblingOrderUUID,
		OriginID:         originID,
		OriginClass:      originClass,
	}
	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex parent for buried reshuffle: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, payloadCode, deliveryNode)
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode)

	// Resolve the lane's parent group so the planner has the group ID
	// for shuffle-slot search and the target_nodes property read.
	lane, err := d.db.GetNode(buried.LaneID)
	if err != nil || lane == nil || lane.ParentID == nil {
		d.dbg("complex: buried lane %d lookup failed (%v) — failing parent %d", buried.LaneID, err, order.ID)
		d.failOrderInternal(order, "reshuffle_error", "cannot determine node group for buried lane")
		return
	}
	groupID := *lane.ParentID

	// Lane-contention: leave the parent Queued for scanner replay.
	if d.laneLock.IsLocked(buried.LaneID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, "lane-locked",
			QueueParams{Lane: lane.Name, Payload: payloadCode})
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}

	// Mode selection: empty target_nodes → expose mode; non-empty →
	// target-node mode (or queue when all targets occupied).
	targetNodeNames := ReshuffleTargetNodes(d.db, lane.ID, groupID)
	var plan *ReshufflePlan
	if len(targetNodeNames) == 0 {
		plan, err = PlanReshuffleUnburyOnly(d.db, buried.Bin, buried.Slot, lane, groupID)
	} else {
		targetNode, allOccupied, terr := d.pickEmptyReshuffleTarget(groupID, targetNodeNames)
		if terr != nil {
			d.failOrderInternal(order, "reshuffle_error", terr.Error())
			return
		}
		if allOccupied {
			d.setQueueReason(order, protocol.QueueStorageRearranging, "targets-occupied",
				QueueParams{Lane: lane.Name, Payload: payloadCode})
			d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
			return
		}
		plan, err = PlanReshuffleToTarget(d.db, buried.Bin, buried.Slot, lane, groupID, targetNode)
	}
	if err != nil {
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot plan reshuffle: %v", err))
		return
	}

	// Race-safe lock acquisition.
	if !d.laneLock.TryLock(buried.LaneID, order.ID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, "lock-race",
			QueueParams{Lane: lane.Name, Payload: payloadCode})
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}

	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID, order.ID)
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot create compound order: %v", err))
		return
	}
	// Expose-mode only: persist the lane-extension entry NOW so the
	// listener at AdvanceCompoundOrder terminal can look up the
	// target bin ID directly instead of re-deriving from lane state.
	// Target-node mode releases the lane immediately at terminal —
	// no row needed.
	if len(targetNodeNames) == 0 {
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
	lane, err := d.db.GetNode(buried.LaneID)
	if err != nil || lane == nil || lane.ParentID == nil {
		d.failOrderInternal(order, "reshuffle_error", "cannot determine node group for buried lane")
		return
	}
	groupID := *lane.ParentID

	if d.laneLock.IsLocked(buried.LaneID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, "lane-locked",
			QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
		return
	}

	targetNodeNames := ReshuffleTargetNodes(d.db, lane.ID, groupID)
	var plan *ReshufflePlan
	if len(targetNodeNames) == 0 {
		plan, err = PlanReshuffleUnburyOnly(d.db, buried.Bin, buried.Slot, lane, groupID)
	} else {
		targetNode, allOccupied, terr := d.pickEmptyReshuffleTarget(groupID, targetNodeNames)
		if terr != nil {
			d.failOrderInternal(order, "reshuffle_error", terr.Error())
			return
		}
		if allOccupied {
			d.setQueueReason(order, protocol.QueueStorageRearranging, "targets-occupied",
				QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
			return
		}
		plan, err = PlanReshuffleToTarget(d.db, buried.Bin, buried.Slot, lane, groupID, targetNode)
	}
	if err != nil {
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot plan reshuffle: %v", err))
		return
	}

	if !d.laneLock.TryLock(buried.LaneID, order.ID) {
		d.setQueueReason(order, protocol.QueueStorageRearranging, "lock-race",
			QueueParams{Lane: lane.Name, Payload: order.PayloadCode})
		return
	}
	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID, order.ID)
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot create compound order on replay: %v", err))
		return
	}
	// Same expose-mode-only persistence as the intake path. See the
	// comment in handleComplexBuriedAtIntake.
	if len(targetNodeNames) == 0 {
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

// pickEmptyReshuffleTarget walks the configured target-node names in
// order and returns the first one with zero bins. Returns
// (nil, true, nil) when all configured targets are occupied — the
// caller queues the parent in that case rather than falling back to
// expose mode. Validation failures (target name doesn't resolve, or
// resolves to a synthetic / lane / non-direct-child) return a
// non-nil error.
func (d *Dispatcher) pickEmptyReshuffleTarget(groupID int64, names []string) (target *nodes.Node, allOccupied bool, err error) {
	if len(names) == 0 {
		return nil, false, nil
	}
	for _, name := range names {
		node, gErr := d.db.GetNodeByDotName(name)
		if gErr != nil || node == nil {
			return nil, false, fmt.Errorf("reshuffle target %s not found in group %d", name, groupID)
		}
		if node.ParentID == nil || *node.ParentID != groupID {
			return nil, false, fmt.Errorf("reshuffle target %s is not a direct child of group %d", name, groupID)
		}
		if node.IsSynthetic || node.NodeTypeCode == protocol.NodeClassLANE {
			return nil, false, fmt.Errorf("reshuffle target %s must be a non-synthetic, non-lane node", name)
		}
		cnt, _ := d.db.CountBinsByNode(node.ID)
		if cnt == 0 && node.ClaimedBy == nil {
			return node, false, nil
		}
	}
	return nil, true, nil
}
