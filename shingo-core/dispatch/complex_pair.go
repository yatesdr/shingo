package dispatch

import (
	"fmt"
	"log"
	"sort"

	"shingo/protocol"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// complex_pair.go — A COORDINATED PAIR IS ONE JOB.
//
// ── THE RULE ──────────────────────────────────────────────────────────────
//
// Both legs of a coordinated multi-leg order are admitted in the SAME scanner
// pass, or neither is. The first refusal — no source, no destination slot, no
// lane, no partner row yet — parks the whole pair, with ONE cause naming what
// the pair is waiting on, holding NOTHING.
//
// ONE EXCEPTION, AND IT IS NOT A REFUSAL: a leg whose bin is buried can take its
// own excavation inside its phases (§R.91) — it takes the lane in its own name,
// goes `reshuffling`, and the dig's first robot is sent. That leg is working,
// not waiting, and parking it would drop the dig's lane lock under a robot
// already driving into the corridor. So a pivot keeps what it took, and only
// its partner parks — on the partner wait, with nothing held — until the dig is
// done and the pivot is acquiring again.
//
// WHAT COUNTS AS THE PAIR. The legs that are acquiring. A partner already
// committed to the fleet, or terminal, leaves a one-leg slice (the completion
// path, or the death rule's). A partner that is none of those — digging its own
// bin out, or not through intake yet — is an incomplete pair, and nothing goes.
// A partner Core REFUSED at intake is not incomplete but impossible: its row will
// never exist, so the leg that names it fails, carrying the refusal.
//
// ── WHY IT IS EXPRESSIBLE AT DISPATCH, AND ONLY HERE ──────────────────────
//
// Dispatch sends a two_robot evac ONE instruction: `wait(LINE)` — drive to the
// line and park. splitAtWait returns steps[:1] for it, and its pickup is
// appended at RELEASE. No bin moves at dispatch, so nothing here can strand a
// line by itself. What dispatch decides is whether the pair is COMMITTED, and
// the operator's own fact is that commitment is the invariant that matters:
// operators release with the supply dispatched but not yet at the line, the evac
// takes the empty out, and the supply arrives shortly after and finishes. A
// supply slightly behind is the intended flow. A supply that was never coming is
// ALN_003 (2026-06-03), and dispatching the pair together makes "never coming"
// unconstructible — if the evac is parked at the line, its supply was committed
// to the fleet no later than it was: in the same pass, or already committed when
// the evac went on its own.
//
// So RELEASE IS UNCHANGED and must stay so. ComputeSwapReady, the RELEASE
// button, the deferral-and-refire machinery and the Edge's press-index lift rule (release.PlanAct's G7)
// are not this file's business and are not touched by it.
//
// ── NO MODE NAMES. NOT ONE ────────────────────────────────────────────────
//
// The rule reads the PAIR STRUCTURE — that the order names a sibling — and never
// `SwapMode`. That is deliberate: two_robot,
// press-index and whatever comes next get the same rule, and there is no
// carve-out for anybody. A `SwapMode ==` in anything below would be the old
// exemption re-spelled. See swap_mode.go's law: a gate reads the steps or a
// declared property, never the mode name.
//
// ── WHAT IT COSTS, SO NOBODY REDISCOVERS IT AS A BUG ──────────────────────
//
// In a cell whose inbound source and outbound destination are the same market,
// the supply's own pickup is what frees the slot the evac needs. Under this rule
// neither leg goes alone, so at 100% capacity such a pair used to wait for
// something else to free a slot.
//
// THE UPGRADE PATH HAS LANDED: the evac's slot may now BE the one the supply
// vacates (vacated_slot.go). The supply acquires first in stage 1, and the evac
// is handed its lift. The evac's drop lands on the slot that lift empties, and
// the release fence holds the evac until the lift has actually happened. SPR
// 2026-09-28, pairs 7060/7061 and 7062/7063, is the case that made it
// necessary.
//
// IT IS NOT FACE 3. Face 3 let one leg go while its partner was blocked; here
// both legs still commit in one pass or neither does, and nothing is exempted.
//
// What still queues at 100%: a drop into a LANE slot (lane holds are per order,
// and two orders on one mouth is the order 22/23 deadlock); a lift that comes
// after the drop (an unflipped press-index R1 drops before it fetches); a
// partner lift behind the partner's own wait (both press-index legs move on the
// same release); and a partner that is not in this pass — a one-leg slice,
// sequential's B, a relay.

// coordinatedPairLegs returns the acquiring legs of order's coordinated pair, in
// ascending order-id order — or, when the pair cannot be evaluated this pass,
// what Core knows of the partner it is missing.
//
// Three answers, and the caller must distinguish all three:
//
//	(nil,  nil)   a solo order. Runs the ordinary per-order phases.
//	(nil,  wait)  an incomplete pair: this order names a partner whose row does
//	              not exist, or whose row is neither acquiring, committed to the
//	              fleet, nor terminal (it is digging its own bin out). Nothing
//	              dispatches. wait.refused is set when Core refused the partner
//	              at intake: its row never will exist, and the leg fails.
//	(legs, nil)   the pair, filtered to the legs still acquiring — one leg when
//	              the partner is committed or terminal.
//
// ── THE POINTER IS PRESENT BEFORE THE ROW IS, AND THAT IS THE WHOLE POINT ─
//
// protocol.ComplexOrderRequest.SiblingOrderUUID rides BOTH legs of a Core pair —
// every Edge door that makes one mints both uuids before it creates either (a
// relay goes unpaired by design; Edge engine/relay_pair.go) — so the pointer is
// never the thing that is missing. What IS missing, for the leg created first,
// is its partner's ORDER ROW: the first leg's door runs its one acquire under
// the scan lock, synchronously, and the partner has not been ingested yet.
//
// The swap-hold gate handled that asymmetrically and on purpose — fail CLOSED
// for an evac ("hold rather than strand the line"), fail OPEN for a filler — so
// a two_robot supply, which is the leg created first, dispatched alone before
// its evac existed. That gate is deleted; this function is what answers the
// question now. TestSwapPeerTerminalRace_LiveLegResolvesDeadSibling pins that
// the window is real, not theoretical.
//
// Under the pair rule that fail-open is a half-dispatched pair, so it goes. The
// releaser is the partner's own intake: its door's acquire asks the pair's
// leader (the lower id, fulfillment Scanner.acquireLocked), the pair is
// complete, both legs go. Bounded, and no new subscription.
// If that intake REFUSES the partner there is no row to wait for: intake records
// the refusal (orders.IntakeRefusal), this function hands it to the caller, and
// the leg fails with the partner's reason.
//
// WHICH LEG IS CREATED FIRST IS NOT A ROLE and must not be read as one:
// two_robot creates the supply first, a press-index CHANGEOVER creates the
// supply first, and a STEADY-STATE press-index swap creates R1 — the evac —
// first. This function never asks. It asks whether both rows exist.
//
// A TERMINAL SIBLING IS NOT A LEG. It is filtered out here rather than special-
// cased, because resolving a dead partner is the peer-terminal handler's job and
// it already runs from the surviving side in applySwapGates. A survivor whose
// partner is dead falls through to a one-leg slice and is resolved there.
func (d *Dispatcher) coordinatedPairLegs(order *orders.Order) ([]*orders.Order, *pairWait) {
	sibUUID, err := d.db.OrderSiblingUUID(order.ID)
	if err != nil {
		// Transient read error — fail OPEN to the solo path, the same way the
		// swap gate has always failed open on this read. Never freeze a robot on
		// a flaky read; the next scanner tick re-asks.
		log.Printf("dispatch: pair lookup for order %d: %v (treating as solo this pass)", order.ID, err)
		return nil, nil
	}
	if sibUUID == "" {
		return nil, nil // not a pair
	}
	// GetOrderByUUID answers a missing row with sql.ErrNoRows, not (nil, nil), so
	// absence is the error's business to say and readFailed's to tell apart.
	sib, sibErr := d.db.GetOrderByUUID(sibUUID)
	if readFailed(sibErr) {
		log.Printf("dispatch: pair partner read for order %d (%s): %v", order.ID, sibUUID, sibErr)
		return nil, &pairWait{} // unreadable partner is an incomplete pair, not a solo order
	}
	if sib == nil {
		// Declared partner, no row. Either Core has not received it yet — a wait
		// its intake ends — or Core refused it and it never will. Only the record
		// intake keeps of a refusal tells the two apart.
		refused, rerr := d.db.GetIntakeRefusal(sibUUID)
		if rerr != nil {
			log.Printf("dispatch: pair partner refusal read for order %d (%s): %v (treating as not yet received)",
				order.ID, sibUUID, rerr)
		}
		return nil, &pairWait{refused: refused}
	}

	// ── ON-READ REPAIR OF THE BIDIRECTIONAL LINK ──────────────────────────
	//
	// If the peer's back-link is missing — a failed intake back-link write, or a
	// Core talking to an OLDER EDGE, which sent the pointer on the second-created
	// leg only — heal it now that both rows exist. Idempotent, and gated on
	// "actually missing" so the happy path re-touches nothing.
	//
	// IT MOVED HERE FROM THE SWAP-HOLD GATE, which is deleted, and this is a
	// better home than the one it had. There it ran only for legs that reached
	// the gate; here it runs for EVERY coordinated leg on every pass, which is
	// the whole population that needs it.
	//
	// It matters more than it did. A one-way link used to disarm a hold; now it
	// decides whether dispatch sees one job or two — an unhealed peer reads as a
	// solo order from its own side and would dispatch alone. The forward pointer
	// this function just read is enough for THIS leg, so the pair still forms;
	// the repair is what stops the PARTNER forming a different answer.
	if sib.SiblingOrderUUID != order.EdgeUUID {
		if _, rerr := d.db.LinkOrderSiblingsByEdgeUUID(order.EdgeUUID, sibUUID); rerr != nil {
			log.Printf("dispatch: swap back-link repair for order %d sib %s: %v", order.ID, sibUUID, rerr)
		}
	}

	// ── A PARTNER THAT IS NOT ACQUIRING IS NOT THEREFORE GONE ─────────────
	//
	// This used to keep the acquiring legs and call whatever was left a solo
	// order. That is right for a partner committed to the fleet (the completion
	// path) and for a dead one (the death rule's), and wrong for everything else:
	// a partner `reshuffling` has pivoted into its own dig and is coming back to
	// acquiring when the dig is done. Read as absent, it let this leg go to the
	// fleet alone — an evac released while its supply digs lifts the line's bin
	// with no replacement committed, which is ALN_003.
	if !protocol.IsAcquiring(sib.Status) && !protocol.IsTerminal(sib.Status) && !pairLegCommitted(sib.Status) {
		return nil, &pairWait{partner: sib}
	}

	legs := make([]*orders.Order, 0, 2)
	for _, leg := range []*orders.Order{order, sib} {
		if protocol.IsAcquiring(leg.Status) {
			legs = append(legs, leg)
		}
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].ID < legs[j].ID })
	return legs, nil
}

// pairWait is what Core knows of the partner an incomplete pair is waiting on.
type pairWait struct {
	// partner is the partner's row, or nil when Core has none.
	partner *orders.Order
	// refused is Core's record of refusing the partner at intake. Non-nil means
	// the row will never exist, so there is nothing to wait for.
	refused *orders.IntakeRefusal
}

// pairLegCommitted reports whether a pair leg has been handed to the fleet: the
// vendor has it (dispatched, in transit, staged, or faulted inside its grace
// period) or it has delivered. Its partner then goes on its own — the pass that
// committed this leg already made the both-or-neither decision.
func pairLegCommitted(s protocol.Status) bool {
	return protocol.IsVendorTracked(s) || s == StatusDelivered
}

// preparedLeg is one leg that has cleared every acquisition phase and is
// waiting for the pair's commit stage, with the resolved steps the fleet create
// needs. It exists only between the two stages of one pass.
type preparedLeg struct {
	order *orders.Order
	steps []resolvedStep
	hand  Hand
}

// dispatchPairInOnePass is the pair rule's body: prepare every leg, and only
// when EVERY leg has cleared, take both legs' hands as one and hand them to the
// fleet.
//
// ── TWO STAGES, AND THE BOUNDARY BETWEEN THEM IS THE RULE ─────────────────
//
// Stage 1 prepares: plan, gates, the destination's and the sources' soft
// reserves, the lane verdict, the hand each leg would take. Stage 2 builds each
// leg's create, and commitPairToFleet takes both hands in one transaction and
// then asks the fleet. Nothing is held hard until that one hand, so aborting
// before it costs a release and a wait, and there is no half-committed state to
// unwind.
//
// ── THE LEADER, AND WHY IT IS A COMPARISON RATHER THAN A COLUMN ───────────
//
// The fulfillment scanner walks ListAcquiringOrders() and calls
// DispatchPreparedComplex once per row, so BOTH legs are visited inside one
// scan(). Without an election the pass would run twice — acquire, park, release,
// then acquire, park, release again — which is correct and wasteful, and makes
// "a repeat pass is a no-op" merely approximately true.
//
// The leg with the lowest id among the legs STILL ACQUIRING drives; the other
// returns the pass's verdict having touched nothing. Derived from durable state
// every pass, so it needs no new column and cannot go stale: if the leader
// dispatches and its partner does not (the fleet refused its create), the
// partner is then the lowest ACQUIRING leg, becomes the leader, and completes
// itself.
//
// ── A ONE-LEG SLICE IS NOT AN ERROR ───────────────────────────────────────
//
// It means the partner is committed to the fleet already, or terminal —
// coordinatedPairLegs parks every other partner state before a slice reaches
// here, so "not in the slice" never means "still on its way". Committed is the
// completion path and the leg should go. Terminal is the death rule's business,
// and applySwapGates runs the peer-terminal unwind from the surviving side, so
// the leg is resolved inside its own phases rather than by a test here.
//
// ── A PIVOT IS NOT A PARK ─────────────────────────────────────────────────
//
// A leg can leave its phases `reshuffling`: its bin was buried and it took its
// own excavation (lane in its own name, the dig's first robot sent). That is
// done=true like a refusal, and parkPair would release its lanes — dropping the
// dig's mouth row under a working robot, which only LaneLock.Unlock may do. So
// the pivot keeps what it took and only its partners park, on the partner wait.
func (d *Dispatcher) dispatchPairInOnePass(self *orders.Order, legs []*orders.Order) error {
	if legs[0].ID != self.ID {
		d.dbg("complex: order %d defers this pass to pair leader %d", self.ID, legs[0].ID)
		return fmt.Errorf("complex order %d: pair led by order %d this pass", self.ID, legs[0].ID)
	}

	// STAGE 1: each leg's soft half, lifters first (stageOneOrder): its plan, its
	// soft reserves, its admission verdict and the hand it would take. The leg
	// prepared first hands the next its plan and the pickups it reserved
	// (pairPass), which the vacated-slot rule reads; nothing is hard yet.
	ready := make([]preparedLeg, 0, len(legs))
	var pc *pairPass
	for _, leg := range stageOneOrder(legs) {
		steps, hand, reserved, st := d.prepareComplexHand(leg, pc)
		if st.done {
			return d.pairLegStopped(legs, leg, st.err)
		}
		ready = append(ready, preparedLeg{order: leg, steps: steps, hand: hand})
		if pc == nil {
			pc = &pairPass{partner: leg, steps: steps, claimed: reserved}
		}
	}

	// STAGE 2: each leg's create, built and not sent.
	commits := make([]*complexCommit, 0, len(ready))
	for _, p := range ready {
		c, err := d.buildComplexCommit(p.order, p.steps, p.hand, WaitOf(p.order))
		if err != nil {
			return d.pairLegStopped(legs, p.order, err)
		}
		commits = append(commits, c)
	}
	return d.commitPairToFleet(legs, commits)
}

// commitPairToFleet sends a pair as ONE JOB (C2b A.2): both legs' hands and
// both `dispatched` writes in one transaction (acquireHands), so the pair holds
// both or neither; then the creates, lifter first. The owner's model is
// "release both or neither", and it is the hand's rule, not the fleet's (P0
// ruling Q7):
//
//   - a hand refused on either leg holds nothing for either: the pair parks on
//     that leg's cause (pairLegStopped), as a refusal in stage 1 does;
//   - the lifter's create refused: nothing of the pair is with the fleet, so
//     both demote through the one door, the return without having been asked;
//   - the lifter accepted and the return refused: the lifter is with the fleet
//     and is never preempted (v4 §2.7); the return demotes alone, holding
//     nothing hard, bound to its sibling, and re-acquires through its own hand
//     on the next pass.
//
// It replaced a stage 1 that confirmed each leg's slots, bins and lane mouths
// in separate transactions and a stage 2 that created each leg with an empty
// hand and returned on the first error: a lifter refused by the fleet left the
// return `pending` holding its stage-1 claims
// (TestVacatedSlot_P12_LifterCreateFailsDropperNotCreated).
func (d *Dispatcher) commitPairToFleet(legs []*orders.Order, commits []*complexCommit) error {
	takes := make([]handTake, 0, len(commits))
	for _, c := range commits {
		if err := d.assertDeclaredEveryLaneItEnters(c.order.ID, c.req, c.entering); err != nil {
			d.refuseComplexHandover(c.order, c.told, err)
			return d.pairLegStopped(legs, c.order, err)
		}
		if d.preHandHook != nil {
			d.preHandHook(c.order.ID)
		}
		takes = append(takes, handTake{order: c.order, hand: c.hand, entering: c.entering, actor: c.actor,
			reason: fmt.Sprintf("vendor order %s created", c.req.OrderID)})
	}
	if i, err := d.acquireHands(takes); err != nil {
		c := commits[i]
		if IsConcurrentTransition(err) {
			// Another caller owns this leg and is dispatching it; the hand rolled
			// back, so nothing of either leg is held. Leave both alone.
			log.Printf("dispatch: pair leg %d was dispatched by another caller first — yielding the pair: %v",
				c.order.ID, err)
			return err
		}
		d.refuseComplexHandover(c.order, c.told, err)
		return d.pairLegStopped(legs, c.order, err)
	}

	for i, c := range commits {
		if err := d.handoverToFleet(c.order, c.req, c.actor); err != nil {
			concurrent := IsConcurrentTransition(err)
			if !concurrent {
				d.ReleaseLaneOccupancy(c.order.ID)
			}
			d.refuseComplexHandover(c.order, c.told, err)
			if i == 0 && !concurrent {
				// Nothing of the pair is with the fleet: the legs after this one
				// were held with it and are let go with it, through the same door.
				for _, rest := range commits[1:] {
					d.ReleaseLaneOccupancy(rest.order.ID)
					code, cause, params := FleetRefusalCause(err, rest.order.DeliveryNode)
					d.DemoteAfterFleetRefusal(rest.order, rest.told, code, cause, params)
				}
			}
			return err
		}
		d.afterComplexCreate(c)
	}
	return nil
}

// pairLegStopped answers a pair whose leg stopped before the pair was sent —
// in its preparation, its build, or its hand — and returns err. Three shapes,
// asked of the leg as it now stands:
//
//   - it ended: the death rule has the pair (HandleSwapPeerTerminal), and the
//     other legs let go of what they reserved;
//   - it pivoted into its own dig: it keeps its lane, and the others wait for it;
//   - it waits: the pair parks on its cause, holding nothing (parkPair).
func (d *Dispatcher) pairLegStopped(legs []*orders.Order, leg *orders.Order, err error) error {
	fresh, ferr := d.db.GetOrder(leg.ID)
	if ferr == nil && fresh != nil && protocol.IsTerminal(fresh.Status) {
		for _, other := range legs {
			if other.ID != leg.ID {
				d.releaseLegHoldings(other, "pair leg died in the pass")
			}
		}
		if kind := swapTerminalKind(fresh.Status); kind != "" {
			d.HandleSwapPeerTerminal(fresh.ID, kind)
		}
		d.dbg("complex: pair leg %d went %s during its pass — the death rule has the pair", leg.ID, fresh.Status)
		return err
	}
	if ferr == nil && fresh != nil && fresh.Status == StatusReshuffling {
		for _, other := range legs {
			if other.ID != leg.ID {
				_ = d.parkPairAwaitingPartner(other, fresh)
			}
		}
		d.dbg("complex: pair leg %d pivoted into its own dig — it keeps its lane; its partner waits", leg.ID)
		return err
	}
	d.parkPair(legs, leg)
	return err
}

// parkPair releases everything the pass acquired for every leg and writes the
// BLOCKED leg's cause onto all of them.
//
// ── ONE PAIR, ONE CAUSE ───────────────────────────────────────────────────
//
// The two-row park with two causes is gone. It existed because the legs were two
// independently-dispatched orders, so each named whatever had stopped IT — one
// row saying "waiting for a slot" and the other "waiting for partner", which is
// two answers to one question and sends the operator to two different places.
//
// The cause written is the one the blocked leg's own phase computed, copied
// verbatim: the rendered sentence, the code and the cause tag. Copying rather
// than re-deriving is deliberate — the arm that made the decision is the only
// thing that can name it, and re-deriving here
// would be a second evaluation against a database that may have moved. It also
// keeps every existing releaser row honest: the pair parks under the cause that
// already has a releaser sentence written for it, instead of a new tag nothing
// knows how to clear.
//
// ── AND IT HOLDS NOTHING ──────────────────────────────────────────────────
//
// Rule 1 says an order holds its partials while its set is incomplete, so a
// wait never re-races for what it already had. Read at the PAIR — which is the
// unit of work now — the partials of an incomplete pair are the whole pair's,
// and the pair does not have them. Keeping them would be strictly worse than
// re-racing: the supply's source bin and the evac's outbound slot are exactly
// what another pair needs to complete and free the slot this one is waiting on,
// so a parked pair squatting on them can wedge the market it is queued behind.
// That is the shape the spare was violating.
//
// Best-effort on the release, and loud when it fails: the reconciliation
// sweeps (ReleaseAcquiringOrphanClaims, the owner-liveness reaper) are the
// backstop for a row that leaks past here, and failing the park because a
// release errored would trade a described wait for an undescribed stall.
func (d *Dispatcher) parkPair(legs []*orders.Order, blocked *orders.Order) {
	for _, leg := range legs {
		d.releaseLegHoldings(leg, "pair park")
	}

	// Re-read the blocked leg: its phase wrote the queue detail through
	// WriteQueueDetail, which updates the in-memory struct too, but the struct
	// the scanner handed us is not necessarily that one (applySwapGates re-reads
	// its own copy). The row is the authority.
	src, err := d.db.GetOrder(blocked.ID)
	if err != nil || src == nil {
		log.Printf("dispatch: pair park — could not re-read blocked order %d for its cause: %v", blocked.ID, err)
		return
	}
	if src.QueueReason == "" {
		// A phase that parked without naming a cause is a defect in that phase,
		// not something to invent an answer for here. Log it rather than write a
		// blank sentence over the partner's row.
		log.Printf("dispatch: pair park — order %d parked with no queue reason; leaving its partner's row alone", blocked.ID)
		return
	}
	// The partner takes the blocked leg's wait as it stands, through the one
	// door: an unchanged row writes nothing, and a partner whose wait changed is
	// told at its station like any other changed wait. A bare store write here
	// left the partner's station reading its older sentence.
	for _, leg := range legs {
		if leg.ID == blocked.ID {
			continue
		}
		writeQueueWait(d.db, log.Printf, "dispatch: pair park", leg, WaitOf(src), d.NotifyWaitChanged)
	}
	d.dbg("complex: pair parked on order %d's cause (%s); %d leg(s) holding nothing",
		blocked.ID, src.QueueCause, len(legs))
}

// releaseLegHoldings hands back everything a pair leg acquired — bin and slot
// claims, reservations, its bin pointer (ReleaseOrderHoldings) and its lane rows
// — without moving its status. Best-effort and loud: the reconciliation sweeps
// are the backstop for a row that leaks past here, and failing a park because a
// release errored would trade a described wait for an undescribed stall.
func (d *Dispatcher) releaseLegHoldings(leg *orders.Order, why string) {
	// THE SLOTS IT HELD ARE READ FIRST: a lane this leg walled only through a
	// slot row (a never-dispatched deeper store, stillComingToLane) is woken
	// once the row is gone (wakeLanesOfSlots), or its dweller waits for the floor.
	var slots []int64
	if rows, err := d.db.ListReservationsByOrder(leg.ID); err == nil {
		for _, r := range rows {
			if r.Kind == reservations.KindSlot {
				slots = append(slots, r.NodeID)
			}
		}
	}
	if err := d.db.ReleaseOrderHoldings(leg.ID); err != nil {
		log.Printf("dispatch: %s — release holdings for order %d: %v (reconciliation will sweep it)", why, leg.ID, err)
	} else {
		d.wakeLanesOfSlots(slots...)
	}
	if err := d.ReleaseLanesForOrder(leg.ID); err != nil {
		log.Printf("dispatch: %s — release lanes for order %d: %v", why, leg.ID, err)
	}
}

// parkPairAwaitingPartner parks a leg whose partner is not ready to go with it:
// Core has no row for it yet (partner nil), or its row is digging its own bin
// out.
//
// The pair cannot be evaluated, so nothing dispatches. It holds nothing for the
// same reason the rest of the rule holds nothing, and it parks under
// CauseSwapHold — which is now that cause's ONLY producer: every other swap wait
// names the physical thing the pair is short of, and this one genuinely is
// waiting on the sibling itself. The sentence says which of the two waits it is,
// because they end at different events.
//
// The releasers are the partner's own transitions: its intake's acquire asks
// the pair's leader, and a finished dig resumes it through `pending` and its
// one acquire. A partner
// refused at intake never lands, and failForRefusedPartner ends the wait.
func (d *Dispatcher) parkPairAwaitingPartner(order, partner *orders.Order) error {
	var partnerStatus protocol.Status
	if partner != nil {
		partnerStatus = partner.Status
	}
	d.releaseLegHoldings(order, "awaiting-partner park")
	d.setQueueReason(order, protocol.QueueWaitingForPartner, CauseSwapHold,
		QueueParams{Sibling: order.SiblingOrderUUID, SiblingStatus: partnerStatus})
	d.dbg("complex: order %d holding — coordinated partner %s is not ready to go with it", order.ID, order.SiblingOrderUUID)
	return fmt.Errorf("complex order %d: waiting for coordinated partner %s", order.ID, order.SiblingOrderUUID)
}

// failForRefusedPartner ends a pair leg whose partner Core refused at intake.
//
// The partner's row will never exist, so a wait for it has no releaser: parked,
// this leg would sit under swap-hold until the anomaly board noticed, a
// congestion-shaped row for what is a fault. It fails instead — as a failure and
// not a peer unwind (TermPartnerRefused) — carrying the partner's own refusal,
// because that refusal is the only account of what went wrong and the refused
// leg left no row to carry it.
//
// Two callers, one per order of events: complex intake, when the refusal lands
// on a leg that is already here (refuseComplexIntake), and this leg's own pass,
// when it arrives after the refusal (DispatchPreparedComplex, from
// coordinatedPairLegs' record).
func (d *Dispatcher) failForRefusedPartner(order *orders.Order, r *orders.IntakeRefusal) error {
	detail := fmt.Sprintf("partner order %s was refused by Core at intake (%s: %s) — this leg cannot go without it",
		r.EdgeUUID, r.ErrorCode, r.Detail)
	d.releaseLegHoldings(order, "partner refused")
	d.failOrderInternal(order, string(protocol.TermPartnerRefused), detail)
	return fmt.Errorf("complex order %d: %s", order.ID, detail)
}
