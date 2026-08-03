package dispatch

import (
	"encoding/json"
	"log"

	"shingo/protocol"
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
	var (
		queueReason string
		queueCode   protocol.QueueCode
		queueCause  string
	)
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
			// to re-attempt. Pick the queue code from the typed kind so a
			// saturated dropoff (slot) and a dry empty pool (material) park
			// under the right operator category without re-sniffing the error.
			resolvedSteps = stepsAsResolved(p.Steps)
			queueCause = "intake-resolve"
			capDetail := capacityDetailFrom(payload)
			queueCode = queueCodeForCapacity(capDetail.kindOf())
			_, intakeDelivery := extractEndpoints(resolvedSteps)
			queueReason = FormatQueueSentence(queueCode,
				queueParamsForCapacity(capDetail, payloadCode, intakeDelivery))
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

	// Intake site 2 of 3 for the demand grain. BOTH LEGS OF A SWAP CARRY THE
	// SAME ORIGIN — one fire of applyConsumePlan is one demand served by two
	// order rows, so Edge sends the pair with one id and Core stores it twice.
	// Counting the legs as two demands would read every swap-mode episode 2x
	// high, which is the ratio the whole surface is built to read.
	originID, originClass := classifyInboundOrigin(p.OriginID, p.OriginClass, stationID, p.OrderUUID)

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
		// Provenance stamp: complex intake is coordinated. The dispatch
		// discriminator (IsCoordinated) reads this column, not StepsJSON.
		Coordinated: true,
		// Durable two-robot swap linkage: persist the supply sibling's UUID
		// in the CreateOrder INSERT itself, so a two-robot evac's pointer to
		// its supply is written atomically with the order and can never be
		// lost by a failed post-create link step (the old ALN_003 fail-open).
		// "" for non-swap orders. The bidirectional back-link (supply→evac) is
		// still reconciled below / on-read.
		SiblingOrderUUID: p.SiblingOrderUUID,
		OriginID:         originID,
		OriginClass:      originClass,
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	if queueReason != "" {
		// Queue detail is written by the transition that queues an order, never
		// at creation — SetOrderQueueDetail is the one way in, and its other
		// callers are all transitions (the planner, complex dispatch, the
		// fulfillment scanner). The order struct has QueueReason/QueueCode/
		// QueueCause fields, but the writer does not persist them and is not
		// meant to; assigning them above would look like it worked and do
		// nothing.
		if err := d.db.SetOrderQueueDetail(order.ID, queueReason, queueCode, queueCause); err != nil {
			log.Printf("dispatch: set initial queue_reason for complex order %d: %v", order.ID, err)
		}
		log.Printf("dispatch: complex order %d queued at intake — %s", order.ID, queueReason)
	}

	// Two-robot swap pairing, back-link reconcile: the forward pointer
	// (evac→supply) is already persisted atomically in CreateOrder above, so
	// the starvation hold no longer depends on this call succeeding. This call
	// additionally records the supply's back-link (supply→evac) — bidirectional
	// via LinkSiblingsByEdgeUUID's CASE — so either leg can find its peer, which
	// the peer-death handler needs. Runs before EmitOrderQueued triggers the
	// synchronous scanner. The supply row already exists (supply is created
	// first). Best-effort on the BACK-link only: a failure here leaves the
	// durable forward link intact and is healed on-read next tick.
	if p.SiblingOrderUUID != "" {
		if _, err := d.db.LinkOrderSiblingsByEdgeUUID(order.EdgeUUID, p.SiblingOrderUUID); err != nil {
			log.Printf("dispatch: link complex order %d sibling %s: %v", order.ID, p.SiblingOrderUUID, err)
		}
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
