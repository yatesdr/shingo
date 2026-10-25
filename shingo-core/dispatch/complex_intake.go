package dispatch

import (
	"encoding/json"
	"log"

	"shingo/protocol"
	"shingocore/dispatch/plan"
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
// Flow, one unit under the scan lock (door.go, F1):
//  1. Validate + resolve steps.
//  2. Create the order born `pending`, like every order (v4 §2.6).
//  3. Ack to edge.
//  4. One acquire: the scanner calls Dispatcher.DispatchPreparedComplex, and
//     the order dispatches, or parks in `queued` with its cause and its station
//     is told. An intake that already knows it waits parks at once.
func (d *Dispatcher) HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	d.door(func() { d.handleComplexOrderRequest(env, p) })
}

func (d *Dispatcher) handleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	stationID := env.Src.Station
	d.dbg("complex order request: station=%s uuid=%s steps=%d", stationID, p.OrderUUID, len(p.Steps))

	if len(p.Steps) == 0 {
		d.refuseComplexIntake(env, p, "invalid_steps", "complex order requires at least one step")
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
	// Now they create the order and park it with the resolver message as
	// queue_reason, and DispatchPreparedComplex re-resolves on each
	// scanner tick. Structural / unknown-action / unknown-node errors
	// still reject synchronously — those aren't fixable by waiting.
	// reservations.Anyone, and it is not a shortcut: the order row does not
	// exist yet, so there is no id a dig could be held for. Every dig excludes
	// it, which is correct — a lane somebody else is excavating is no place to
	// resolve a brand-new order into. The asker starts mattering on replay
	// (complex_dispatch.go), where the parent may own a lock.
	resolvedSteps, err := d.pure.ComplexSteps(p.Steps,
		SourceNeed{PayloadCode: payloadCode, Asker: reservations.Anyone, ProcessNode: p.ProcessNode})
	var (
		queueParams QueueParams
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
			// ordinary complex parent — born pending from this envelope,
			// same as every other one — so it takes the same path down to
			// the persist as the capacity case beside it. What differs is
			// what happens AFTER: instead of handing off to the scanner,
			// the tail plans and dispatches an unbury (or unbury+retrieve)
			// compound and pivots the parent to Reshuffling. When that
			// compound completes the parent resumes back to pending and its
			// one acquire runs the original first pickup against
			// the now-accessible slot.
			//
			// Preserve the original NGRP-bearing step shape so the resume
			// path (parent -> pending -> acquire -> reResolveComplexStepsFor)
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
			capDetail := capacityDetailFrom(payload)
			queueCause = causeForCapacity(capDetail.kindOf(), CauseIntakeResolve)
			queueCode = queueCodeForCapacity(capDetail.kindOf())
			_, intakeDelivery := extractEndpoints(resolvedSteps)
			queueParams = queueParamsForCapacity(capDetail, payloadCode, intakeDelivery)
		default:
			// Structural / transient / fatal — terminal at intake.
			d.refuseComplexIntake(env, p, "resolution_failed", err.Error())
			return
		}
	}

	stepsJSON, err := json.Marshal(resolvedSteps)
	if err != nil {
		d.refuseComplexIntake(env, p, "internal_error", "failed to marshal steps")
		return
	}

	order := row(complexNeed(stationID, p, resolvedSteps, string(stepsJSON)), plan.Plan{})

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
		d.refuseComplexIntake(env, p, lerr.Code, lerr.Detail)
		return
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex order: %v", err)
		d.refuseComplexIntake(env, p, "internal_error", err.Error())
		return
	}
	if queueCode != "" {
		// Queue detail is written after creation, never by it: CreateOrder does
		// not persist the QueueReason/QueueCode/QueueCause fields, so assigning
		// them above would look like it worked and do nothing. It goes through
		// the one door rather than the bare store call so the struct carries the
		// wait too: the announcement below reads it, and the push after the scan
		// compares against it (WaitTold). This is the order's first wait, so the
		// door tells nobody; the announcement does.
		d.setQueueReason(order, queueCode, queueCause, queueParams)
		log.Printf("dispatch: complex order %d parked at intake — %s", order.ID, order.QueueReason)
	}

	// Two-robot swap pairing, back-link reconcile: the forward pointer
	// (evac→supply) is already persisted atomically in CreateOrder above, so
	// PAIR RECOGNITION no longer depends on this call succeeding — which matters
	// more now than when it was written: the pointer used to arm a hold, and it
	// now decides whether dispatch sees a pair at all (complex_pair.go). This call
	// additionally records the supply's back-link (supply→evac) — bidirectional
	// via LinkSiblingsByEdgeUUID's CASE — so either leg can find its peer, which
	// the peer-death handler needs. Runs before the acquire triggers the
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
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, payloadCode, order.DeliveryNode)

	// Ack to edge before the acquire so the edge's order-table row exists when
	// the dispatched-event fires (the acquire dispatches synchronously, and the
	// edge needs to have already recorded the order ID).
	d.sendAck(env, order.EdgeUUID, order.ID, order.SourceNode)

	// A buried parent does not go to the scanner: it has nothing the scanner
	// could act on until the blocking bin has been moved. The reshuffle tail
	// takes it from here and either dispatches the unbury compound or parks the
	// parent for a later replay.
	if buried != nil {
		d.dbg("complex: order %s buried at intake — bin %d in lane %d (slot %s)",
			p.OrderUUID, buried.Bin.ID, buried.LaneID, buried.Slot.Name)
		d.planBuriedReshuffleAtIntake(order, payloadCode, stationID, buried)
		return
	}

	// An intake that already knows it waits parks with its cause; otherwise the
	// one acquire, under the lock this door holds: dispatched before this
	// returns, or parked with the blocking signal on the row.
	if queueCode != "" {
		d.parkBorn(order)
		return
	}
	d.acquireBorn(order, nil)
}

// complexNeed is the row complex intake writes, stated as a need: the facts
// the request carries, its endpoints and steps as resolved at intake, and its
// coordinated part (ComplexSpec). It is issued pending like every order (v4
// §2.6; C1.1.1): its hand is not whole at its issue — resolved steps and no
// bins — and its one acquire is what goes looking, resting in `queued` with its
// cause when the hand cannot be had. The dropoff-capacity gate counts HOLDERS,
// not statuses (orders.InFlightForDropoffSQL).
//
// BOTH LEGS OF A SWAP CARRY THE SAME ORIGIN (intake site 2 of 3 for the demand
// grain): one fire of applyConsumePlan is one demand served by two order rows,
// so the Edge sends the pair with one id and Core stores it twice. Counting the
// legs as two demands would read every swap-mode episode 2x high, which is the
// ratio the whole surface is built to read.
func complexNeed(stationID string, p *protocol.ComplexOrderRequest, steps []resolvedStep, stepsJSON string) Need {
	source, delivery := extractEndpoints(steps)
	originID, originClass := classifyInboundOrigin(p.OriginID, p.OriginClass, stationID, p.OrderUUID)
	return Need{
		Type:         OrderTypeComplex,
		Station:      stationID,
		Ref:          p.OrderUUID,
		Quantity:     p.Quantity,
		Priority:     p.Priority,
		Desc:         p.PayloadDesc,
		PayloadCode:  p.PayloadCode,
		OriginID:     originID,
		OriginClass:  originClass,
		StationKnows: true,
		Source:       source,
		Delivery:     delivery,
		Steps:        stepsJSON,
		Complex: &ComplexSpec{ProcessNode: p.ProcessNode, Sibling: p.SiblingOrderUUID,
			KeyRoute: p.KeyRoute, KeyTask: p.KeyTask},
	}
}

// refuseComplexIntake answers a complex request Core will not ingest.
//
// For a solo request the reply is the whole of it: a refusal leaves nothing
// behind. For one leg of a pair it is not, because the other leg names this one
// and would otherwise wait for a row that will never exist. So a refused request
// that names a sibling is RECORDED (orders.IntakeRefusal), and a partner that is
// already here, names this leg and is still acquiring fails on the spot, with
// this refusal as its reason. A partner that arrives later, or is digging its own
// bin out, finds the record on its next pass (coordinatedPairLegs).
//
// A ROW UNDER THIS UUID MEANS IT WAS NOT REFUSED. A resend of a request Core
// already took fails its create on the duplicate uuid and lands here, but the
// leg it names was accepted; failing its partner over a duplicate would kill a
// live pair. Neither the record nor the partner is touched then.
func (d *Dispatcher) refuseComplexIntake(env *protocol.Envelope, p *protocol.ComplexOrderRequest, code, detail string) {
	d.sendError(env, p.OrderUUID, code, detail)
	if p.SiblingOrderUUID == "" {
		return
	}
	// A missing row comes back as sql.ErrNoRows, which is the answer this wants;
	// any other error means it cannot be told, and the partner is left alone.
	if own, err := d.db.GetOrderByUUID(p.OrderUUID); readFailed(err) || own != nil {
		return
	}
	if err := d.db.RecordIntakeRefusal(p.OrderUUID, env.Src.Station, code, detail); err != nil {
		log.Printf("dispatch: record intake refusal of %s (pair with %s): %v", p.OrderUUID, p.SiblingOrderUUID, err)
	}
	partner, err := d.db.GetOrderByUUID(p.SiblingOrderUUID)
	if err != nil || partner == nil || partner.SiblingOrderUUID != p.OrderUUID || !protocol.IsAcquiring(partner.Status) {
		return
	}
	_ = d.failForRefusedPartner(partner, &orders.IntakeRefusal{
		EdgeUUID: p.OrderUUID, StationID: env.Src.Station, ErrorCode: code, Detail: detail,
	})
}
