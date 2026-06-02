package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// HandleComplexOrderRequest processes a multi-step transport order from
// edge. Phase 4b of bin-transit-state moved this from "dispatch
// synchronously" to "queue-then-let-scanner-dispatch" so that the
// dropoff-capacity gate in fulfillment.Scanner.tryFulfill is the single
// sync point for capacity decisions across fresh-intake AND queue-
// replay paths. See dispatch/capacity.go for the rationale (race
// between two concurrent fresh intakes + scanner targeting the same
// dropoff would otherwise have a TOCTOU window).
//
// Flow:
//  1. Validate + resolve steps.
//  2. Create order with status=queued (was: pending + immediate dispatch).
//  3. Ack to edge.
//  4. Emit EventOrderQueued — scanner subscribes and runs immediately.
//     Scanner.tryFulfill calls Dispatcher.DispatchPreparedComplex when
//     capacity is green; leaves it queued otherwise.
//
// The latency cost on the happy path is ~milliseconds (event-driven
// scanner trigger, runs synchronously on the emitter goroutine).
// Complex orders briefly transition through `queued` status even when
// capacity is fine; consumers that only watch terminal states are
// unaffected.
func (d *Dispatcher) HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	stationID := env.Src.Station
	d.dbg("complex order request: station=%s uuid=%s steps=%d", stationID, p.OrderUUID, len(p.Steps))

	if len(p.Steps) == 0 {
		d.sendError(env, p.OrderUUID, "invalid_steps", "complex order requires at least one step")
		return
	}

	payloadCode := p.PayloadCode

	// Resolve steps up-front so the scanner doesn't have to re-resolve
	// on the happy path (NGRP children may shift between intake and
	// dispatch — locking the choice at intake is the original
	// optimization).
	//
	// Round-3 follow-up: capacity-shaped resolution failures
	// ("no available slot in node group X", "no bin of requested
	// payload in node group X") used to terminal-reject the order at
	// intake — Edge got an error, no Core-side row created, no retry.
	// Now they create the order as queued with the resolver message as
	// queue_reason, and DispatchPreparedComplex re-resolves on each
	// scanner tick. Structural / unknown-action / unknown-node errors
	// still reject synchronously — those aren't fixable by waiting.
	resolvedSteps, err := d.resolveComplexSteps(p.Steps, payloadCode)
	var queueReason string
	if err != nil {
		class, payload := classifyResolutionError(err)
		switch class {
		case ResolutionBuried:
			// Route to the reshuffle path: create the parent at
			// Queued, pivot to Reshuffling, plan + dispatch an
			// unbury (or unbury+retrieve) compound. When the
			// compound completes the parent resumes back to Queued
			// and the fulfillment scanner runs the original first
			// pickup against the now-accessible slot.
			d.handleComplexBuriedAtIntake(env, p, payloadCode, payload.(*BuriedError))
			return
		case ResolutionCapacity:
			// Capacity-shaped — preserve the original step shape (NGRP
			// names intact) so the replay path has the input it needs
			// to re-attempt.
			resolvedSteps = stepsAsResolved(p.Steps)
			queueReason = err.Error()
		default:
			// Structural / transient / fatal — terminal at intake.
			d.sendError(env, p.OrderUUID, "resolution_failed", err.Error())
			return
		}
	}

	stepsJSON, err := json.Marshal(resolvedSteps)
	if err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to marshal steps")
		return
	}

	sourceNode, deliveryNode := extractEndpoints(resolvedSteps)

	order := &orders.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued, // status-first queueing — scanner picks it up
		Quantity:     p.Quantity,
		Priority:     p.Priority,
		PayloadCode:  payloadCode,
		PayloadDesc:  p.PayloadDesc,
		SourceNode:   sourceNode,
		DeliveryNode: deliveryNode,
		ProcessNode:  p.ProcessNode,
		StepsJSON:    string(stepsJSON),
		QueueReason:  queueReason,
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	if queueReason != "" {
		// CreateOrder may not persist QueueReason depending on the store
		// helper's INSERT column list — set it explicitly so the field is
		// visible to the scanner's queue-reason check and to the HMI.
		if err := d.db.SetOrderQueueReason(order.ID, queueReason); err != nil {
			log.Printf("dispatch: set initial queue_reason for complex order %d: %v", order.ID, err)
		}
		log.Printf("dispatch: complex order %d queued at intake — %s", order.ID, queueReason)
	}
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, payloadCode, deliveryNode)

	// Ack to edge before triggering the scanner so the edge's order-table
	// row exists when the dispatched-event fires (if scanner dispatches
	// synchronously, the edge needs to have already recorded the order ID).
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode)

	// EventOrderQueued is the scanner trigger — wired in engine/wiring.go.
	// Scanner.RunOnce is invoked synchronously on this goroutine via the
	// EventBus; if capacity is green and bins claimable, dispatch happens
	// before this function returns. Otherwise the order sits queued with
	// queue_reason set to the blocking signal.
	d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
}

// DispatchPreparedComplex performs the side-effecting tail of complex-
// order dispatch: claim bins per pickup step, transition the order
// queued → sourcing, send blocks to the fleet, transition → dispatched.
//
// Idempotent prerequisites: the order must have StepsJSON populated
// (intake side stores it on creation) and be in StatusQueued. Caller
// is responsible for the capacity gate — this method assumes green-
// light and proceeds with the atomic claim + dispatch.
//
// Called from:
//   - fulfillment.Scanner.tryFulfill on EventOrderQueued (fresh intake
//     just called HandleComplexOrderRequest)
//   - fulfillment.Scanner.tryFulfill on EventBinUpdated /
//     EventBinEnteredTransit / EventOrderCompleted etc. (slot vacancy
//     unblocks a previously-blocked order)
//
// Errors land on lifecycle.Fail — the order moves to terminal `failed`
// rather than back to queued, since these are unrecoverable from the
// scanner's perspective (steps unparseable, bins unavailable, fleet
// rejects).

// isConcreteStorageDropoff reports whether a delivery node is a concrete
// (non-synthetic) STORAGE/STAGING slot — a direct child of a LANE or NGRP.
// This is the role gate for the complex dropoff-capacity check (#1): such a
// slot must queue-on-full, whereas a LINE/production dropoff must NOT be
// gated (a two-robot supply leg delivers to a line a sibling evac clears, and
// gating it deadlocks). Mirrors engine.isStorageSlot's parent-type rule minus
// the synthetic-root cases — NGRP/LANE dropoffs are handled by step
// re-resolution / ResolutionCapacity before this point.
func (d *Dispatcher) isConcreteStorageDropoff(deliveryNode string) bool {
	if deliveryNode == "" {
		return false
	}
	node, err := d.db.GetNodeByDotName(deliveryNode)
	if err != nil || node == nil || node.IsSynthetic || node.ParentID == nil {
		return false
	}
	parent, err := d.db.GetNode(*node.ParentID)
	if err != nil || parent == nil {
		return false
	}
	return parent.NodeTypeCode == "LANE" || parent.NodeTypeCode == "NGRP"
}

func (d *Dispatcher) DispatchPreparedComplex(order *orders.Order) error {
	// Defense-in-depth: the fulfillment scanner's tryFulfill already
	// gates on StatusQueued before calling here, so a parent in
	// Reshuffling (with a compound in flight) won't reach us through
	// the scanner. Anything that calls this function directly (engine
	// recovery, future call sites) must still respect the invariant —
	// proceeding on a non-Queued order would re-dispatch a parent
	// mid-reshuffle or post-resume races. Return nil so the caller
	// treats it as a no-op rather than an error.
	if order.Status != StatusQueued {
		d.dbg("complex: DispatchPreparedComplex called with status=%s (want queued); skipping", order.Status)
		return nil
	}

	var resolvedSteps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &resolvedSteps); err != nil {
		d.failOrderInternal(order, "invalid_steps", fmt.Sprintf("parse stored steps: %v", err))
		return err
	}

	// Round-3 follow-up: re-resolve any step that still references an
	// NGRP. This happens on the deferred path — intake queued the order
	// because the NGRP was saturated; the scanner replays after slot
	// vacancy events, and we attempt resolution again here. On capacity
	// failure, set queue_reason to the current resolver message and
	// stay queued (don't fail). On other resolver errors, fail with
	// invalid_steps. On success, persist the locked-in concrete-child
	// names so subsequent ticks don't redo the work.
	newSteps, changed, rerr := d.reResolveComplexSteps(resolvedSteps, order.PayloadCode)
	if rerr != nil {
		class, payload := classifyResolutionError(rerr)
		switch class {
		case ResolutionBuried:
			// Multi-burial scenario: a second-or-later step in the
			// order hit a burial after the first compound completed.
			// Same planner the intake path uses.
			buriedErr := payload.(*BuriedError)
			d.dbg("complex: order %d buried at replay — bin %d in lane %d", order.ID, buriedErr.Bin.ID, buriedErr.LaneID)
			d.handleComplexBuriedOnReplay(order, buriedErr)
			return rerr
		case ResolutionCapacity:
			reason := rerr.Error()
			if order.QueueReason != reason {
				if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
					log.Printf("dispatch: set queue_reason for complex order %d: %v", order.ID, serr)
				}
			}
			d.dbg("complex: order %d still capacity-blocked at NGRP resolution: %s", order.ID, reason)
			return rerr
		default:
			d.failOrderInternal(order, "invalid_steps", rerr.Error())
			return rerr
		}
	}
	if changed {
		stepsJSON, mErr := json.Marshal(newSteps)
		if mErr == nil {
			if uErr := d.db.UpdateOrderStepsJSON(order.ID, string(stepsJSON)); uErr != nil {
				log.Printf("dispatch: update steps_json for complex order %d: %v", order.ID, uErr)
			} else {
				order.StepsJSON = string(stepsJSON)
			}
		}
		// Endpoints may have shifted (NGRP→child). Re-extract and persist
		// so handler-side lookups (process_node lookup, source/delivery
		// rendering) reflect the resolved choice.
		newSource, newDelivery := extractEndpoints(newSteps)
		if newSource != order.SourceNode {
			if err := d.db.UpdateOrderSourceNode(order.ID, newSource); err != nil {
				log.Printf("dispatch: update source_node for complex order %d: %v", order.ID, err)
			} else {
				order.SourceNode = newSource
			}
		}
		if newDelivery != order.DeliveryNode {
			if err := d.db.UpdateOrderDeliveryNode(order.ID, newDelivery); err != nil {
				log.Printf("dispatch: update delivery_node for complex order %d: %v", order.ID, err)
			} else {
				order.DeliveryNode = newDelivery
			}
		}
	}
	resolvedSteps = newSteps

	// #1 (regression 2b05dce): restore the dropoff-capacity gate for complex
	// orders, but ONLY for concrete STORAGE/STAGING dropoffs. The scanner
	// dropped the gate for every complex order to unstick two-robot SUPPLY
	// legs — which deliver to a LINE node a sibling EVAC clears, and Core has
	// no SiblingOrderID to model that — but that also let a changeover
	// drop/evac to a FULL concrete storage slot dispatch into the occupied
	// slot. Gate by node role (storage slot = child of LANE/NGRP), NOT by
	// same-order pickup: gating the line case would re-create the deadlock
	// 2b05dce fixed. NGRP dropoffs are already covered above by
	// reResolveComplexSteps / ResolutionCapacity. Stay queued by returning an
	// error — the scanner keeps the order queued and replays it on the next
	// slot-vacancy tick (same contract as the claim_failed branch below).
	if d.isConcreteStorageDropoff(order.DeliveryNode) {
		if blocked, reason := CheckDropoffCapacity(d.db, order.DeliveryNode, order.ID); blocked {
			if order.QueueReason != reason {
				if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
					log.Printf("dispatch: set queue_reason for complex order %d: %v", order.ID, serr)
				}
			}
			d.dbg("complex: order %d queued — concrete storage dropoff %s blocked: %s", order.ID, order.DeliveryNode, reason)
			return fmt.Errorf("dropoff capacity: %s", reason)
		}
	}

	// Claim bins at pickup nodes. RemainingUOP is intentionally nil here
	// — Edge's `CreateComplexOrder` doesn't thread it through at intake,
	// and the operator's release-time RemainingUOP arrives via
	// HandleOrderRelease. If a future Edge starts sending RemainingUOP at
	// intake we'd persist it on the order row to recover here.
	if err := d.claimComplexBins(order, resolvedSteps, order.PayloadCode, nil); err != nil {
		// Three terminal outcomes, distinguished by planningError.Code:
		//   - claim_failed: transient race loss. Don't fail the order;
		//     queue_reason="claim_failed" replays on the next scanner tick.
		//   - no_source_bin: every pickup node was empty. The work was
		//     never needed (e.g. evac for a bin that was removed to
		//     quality hold before dispatch). Route to lifecycle.Skip so
		//     the operator surface treats it as a no-op rather than an
		//     alarm; Edge's HandleOrderSkipped advances the linked
		//     changeover node task to its post-completion state.
		//   - no_bin (default): bins existed but were unclaimable
		//     (already-claimed, payload mismatch, status). Terminal Fail.
		var pe *planningError
		if errors.As(err, &pe) {
			switch pe.Code {
			case "claim_failed":
				if serr := d.db.SetOrderQueueReason(order.ID, "claim_failed"); serr != nil {
					log.Printf("dispatch: set queue_reason claim_failed for order %d: %v", order.ID, serr)
				}
				d.dbg("complex: order %d held in queue on claim_failed: %s", order.ID, pe.Detail)
				return err
			case "no_source_bin":
				d.skipOrderInternal(order, "no_source_bin", pe.Detail)
				return err
			}
		}
		d.failOrderInternal(order, "no_bin", err.Error())
		return err
	}

	preWait, hasWait := splitAtWait(resolvedSteps)
	vendorOrderID := fmt.Sprintf("%s%d-%s", VendorIDPrefix, order.ID, uuid.New().String()[:8])
	blocks := stepsToBlocks(vendorOrderID, preWait, 0)

	if len(blocks) == 0 {
		d.failOrderInternal(order, "invalid_steps", "no actionable steps before wait")
		return fmt.Errorf("no actionable blocks")
	}

	if err := d.lifecycle.MoveToSourcing(order, "scanner", "complex order ready to dispatch"); err != nil {
		log.Printf("dispatch: complex order %d → sourcing: %v", order.ID, err)
	}

	req := fleet.StagedOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   order.Priority,
	}
	d.dbg("complex: creating staged order %s with %d initial blocks (hasWait=%v)", vendorOrderID, len(blocks), hasWait)
	if _, err := d.backend.CreateStagedOrder(req); err != nil {
		log.Printf("dispatch: fleet create staged order failed: %v", err)
		d.failOrderInternal(order, "fleet_failed", err.Error())
		return err
	}
	if !hasWait {
		// No wait — fleet can complete the order immediately.
		if err := d.backend.ReleaseOrder(vendorOrderID, nil, true); err != nil {
			log.Printf("dispatch: fleet mark complete failed: %v", err)
		}
	}

	log.Printf("dispatch: complex order %d dispatched as %s (%d steps)", order.ID, vendorOrderID, len(resolvedSteps))
	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		log.Printf("dispatch: update order %d vendor: %v", order.ID, err)
	}
	if err := d.lifecycle.Dispatch(order, vendorOrderID, "scanner"); err != nil {
		log.Printf("dispatch: complex order %d → dispatched: %v", order.ID, err)
	}
	// Successful dispatch — clear any stale queue_reason from a prior
	// blocked replay attempt.
	if order.QueueReason != "" {
		if err := d.db.SetOrderQueueReason(order.ID, ""); err != nil {
			log.Printf("dispatch: clear queue_reason for order %d: %v", order.ID, err)
		}
	}
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, order.SourceNode, order.DeliveryNode)
	return nil
}

// failOrderInternal is the scanner-path failure helper. Same as
// failOrder but doesn't take an envelope (no edge-bound reply — the
// edge already has the queued status from intake; it'll learn about
// the failure via EventOrderFailed → edge_handler.HandleOrderError).
func (d *Dispatcher) failOrderInternal(order *orders.Order, code, detail string) {
	if err := d.lifecycle.Fail(order, order.StationID, code, detail); err != nil {
		log.Printf("dispatch: fail order %d: %v", order.ID, err)
	}
	d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, order.StationID, code, detail)
}

// skipOrderInternal is the scanner-path "the work was never needed" helper.
// Parallel shape to failOrderInternal but routes through lifecycle.Skip
// (which writes status='skipped' via SkipOrderAtomic, no anomaly mark on
// any leaked claims) and emits EventOrderSkipped. Edge subscribes via
// HandleOrderSkipped and advances the linked changeover node task without
// surfacing a failure to the operator.
func (d *Dispatcher) skipOrderInternal(order *orders.Order, code, detail string) {
	if err := d.lifecycle.Skip(order, order.StationID, code, detail); err != nil {
		log.Printf("dispatch: skip order %d: %v", order.ID, err)
	}
	d.emitter.EmitOrderSkipped(order.ID, order.EdgeUUID, order.StationID, code, detail)
}

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
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on lane contention: %v", serr)
		}
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
			reason := "all reshuffle targets occupied"
			if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
				log.Printf("dispatch: set queue_reason on targets-occupied: %v", serr)
			}
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
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on TryLock race: %v", serr)
		}
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}

	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID)
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
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on replay lane contention: %v", serr)
		}
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
			reason := "all reshuffle targets occupied"
			if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
				log.Printf("dispatch: set queue_reason on replay targets-occupied: %v", serr)
			}
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
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on replay TryLock race: %v", serr)
		}
		return
	}
	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID)
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
		if node.IsSynthetic || node.NodeTypeCode == "LANE" {
			return nil, false, fmt.Errorf("reshuffle target %s must be a non-synthetic, non-lane node", name)
		}
		cnt, _ := d.db.CountBinsByNode(node.ID)
		if cnt == 0 && node.ClaimedBy == nil {
			return node, false, nil
		}
	}
	return nil, true, nil
}
