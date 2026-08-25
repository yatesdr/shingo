package dispatch

import (
	"encoding/json"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
	"shingocore/store/reservations"
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
	// reservations.Anyone, and it is not a shortcut: the order row does not
	// exist yet, so there is no id a dig could be held for. Every dig excludes
	// it, which is correct — a lane somebody else is excavating is no place to
	// resolve a brand-new order into. The asker starts mattering on replay
	// (complex_dispatch.go), where the parent may own a lock.
	resolvedSteps, err := d.resolveComplexSteps(p.Steps, payloadCode, reservations.Anyone)
	var (
		queueReason string
		queueCode   protocol.QueueCode
		queueCause  QueueCause
		// buried non-nil selects the reshuffle tail below. The parent row
		// itself is the same row either way, which is why this is a flag and
		// not a second creation site.
		buried *BuriedError
	)
	if err != nil {
		class, payload := classifyResolutionError(err)
		switch class {
		case ResolutionBuried:
			// The source bin is behind another bin. The parent is still an
			// ordinary complex parent — created Queued from this envelope,
			// same as every other one — so it takes the same path down to
			// the persist as the capacity case beside it. What differs is
			// what happens AFTER: instead of handing off to the scanner,
			// the tail plans and dispatches an unbury (or unbury+retrieve)
			// compound and pivots the parent to Reshuffling. When that
			// compound completes the parent resumes back to Queued and the
			// fulfillment scanner runs the original first pickup against
			// the now-accessible slot.
			//
			// Preserve the original NGRP-bearing step shape so the resume
			// path (parent -> Queued -> scanner -> reResolveComplexSteps)
			// has the input it needs to re-resolve — the same reason the
			// capacity case below does it.
			buried = payload.(*BuriedError)
			resolvedSteps = stepsAsResolved(p.Steps)
		case ResolutionCapacity:
			// Capacity-shaped — preserve the original step shape (NGRP
			// names intact) so the replay path has the input it needs
			// to re-attempt. Pick the queue code from the typed kind so a
			// saturated dropoff (slot) and a dry empty pool (material) park
			// under the right operator category without re-sniffing the error.
			resolvedSteps = stepsAsResolved(p.Steps)
			queueCause = CauseIntakeResolve
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
		// The claim's SEER routing hints, persisted with the order because
		// dispatch happens later — see domain.Order.KeyRoute. Empty on every
		// order until an Edge claim configures one.
		KeyRoute:    p.KeyRoute,
		KeyTask:     p.KeyTask,
		OriginID:    originID,
		OriginClass: originClass,
	}

	// Do the things this order names exist? The wire door has always asked; this
	// one never did, so an order naming a payload Core does not have got a row,
	// an announcement, and an ACK telling the Edge it was accepted — and then
	// waited for material that cannot arrive.
	//
	// The checks only. Not the synthetic-destination resolution that sits beside
	// them at the other door: that rewrites where the order is going, and this
	// order's steps have already chosen the concrete nodes its robot will visit.
	//
	// Before the insert, so a refusal leaves nothing behind, and well before the
	// ack — the ack is the part that made this worse than a slow failure.
	if _, lerr := d.lifecycle.checkOrderRefs(order); lerr != nil {
		d.sendError(env, p.OrderUUID, lerr.Code, lerr.Detail)
		return
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
		if err := d.db.SetOrderQueueDetail(order.ID, queueReason, queueCode, string(queueCause)); err != nil {
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
	//
	// The buried path is excluded because it always has been: when it built its
	// own parent it never made this call, and folding the two creation sites is
	// not the place to start. Whether that exclusion was ever a decision is a
	// separate question — the forward link is written either way and the
	// back-link is healed on read, so the gap is bounded — but it is a real
	// difference between the two paths and it is now visible in one place
	// instead of hidden by them being two functions.
	if p.SiblingOrderUUID != "" && buried == nil {
		if _, err := d.db.LinkOrderSiblingsByEdgeUUID(order.EdgeUUID, p.SiblingOrderUUID); err != nil {
			log.Printf("dispatch: link complex order %d sibling %s: %v", order.ID, p.SiblingOrderUUID, err)
		}
	}
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, payloadCode, deliveryNode)

	// Ack to edge before triggering the scanner so the edge's order-table
	// row exists when the dispatched-event fires (if scanner dispatches
	// synchronously, the edge needs to have already recorded the order ID).
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode)

	// A buried parent does not go to the scanner: it has nothing the scanner
	// could act on until the blocking bin has been moved. The reshuffle tail
	// takes it from here and either dispatches the unbury compound or leaves
	// the parent queued for a later replay.
	if buried != nil {
		d.dbg("complex: order %s buried at intake — bin %d in lane %d (slot %s)",
			p.OrderUUID, buried.Bin.ID, buried.LaneID, buried.Slot.Name)
		d.planBuriedReshuffleAtIntake(order, payloadCode, stationID, buried)
		return
	}

	// EventOrderQueued is the scanner trigger — wired in engine/wiring.go.
	// Scanner.RunOnce is invoked synchronously on this goroutine via the
	// EventBus; if capacity is green and bins claimable, dispatch happens
	// before this function returns. Otherwise the order sits queued with
	// queue_reason set to the blocking signal.
	d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
}
