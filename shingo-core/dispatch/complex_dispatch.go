package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet"
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
//   1. Validate + resolve steps.
//   2. Create order with status=queued (was: pending + immediate dispatch).
//   3. Ack to edge.
//   4. Emit EventOrderQueued — scanner subscribes and runs immediately.
//      Scanner.tryFulfill calls Dispatcher.DispatchPreparedComplex when
//      capacity is green; leaves it queued otherwise.
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
		if !isCapacityResolutionError(err) {
			d.sendError(env, p.OrderUUID, "resolution_failed", err.Error())
			return
		}
		// Capacity-shaped — preserve the original step shape (NGRP names
		// intact) so the replay path has the input it needs to re-attempt.
		resolvedSteps = stepsAsResolved(p.Steps)
		queueReason = err.Error()
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
func (d *Dispatcher) DispatchPreparedComplex(order *orders.Order) error {
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
		if isCapacityResolutionError(rerr) {
			reason := rerr.Error()
			if order.QueueReason != reason {
				if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
					log.Printf("dispatch: set queue_reason for complex order %d: %v", order.ID, serr)
				}
			}
			d.dbg("complex: order %d still capacity-blocked at NGRP resolution: %s", order.ID, reason)
			return rerr
		}
		d.failOrderInternal(order, "invalid_steps", rerr.Error())
		return rerr
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
