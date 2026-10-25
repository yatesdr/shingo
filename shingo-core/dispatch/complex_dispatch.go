package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"

	"shingo/protocol"
	"shingocore/dispatch/plan"
	"shingocore/fleet"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dispatchStep carries a phase helper's decision back to the DispatchPreparedComplex
// orchestrator. done=true means the phase parked, failed, or skipped the order and
// the orchestrator must return err verbatim; done=false (the zero value) means the
// phase completed and control proceeds to the next phase. It lives only at the
// orchestrator boundary — phases that consume the package's native reserveOutcome
// enum collapse it to a dispatchStep at their return rather than surfacing it up.
type dispatchStep struct {
	done bool
	err  error
	// byChoice marks a stop the plan's own group choices caused (a group with no
	// room for the choice as made), the one preparation stop a re-made choice can
	// cure (remakeAfter).
	byChoice bool
	// exchanged marks a stop where a claim exchange answered a burial
	// (claim_exchange.go): nothing was parked, and the demand is acquired again
	// at once, on the plan and the hold the exchange wrote.
	exchanged bool
}

// DispatchPreparedComplex performs the side-effecting tail of complex-
// order dispatch: claim bins per pickup step, send blocks to the fleet,
// transition → dispatched. A complex order is born `pending` like every order;
// a pass that cannot take its whole hand writes the cause and the scanner parks
// it in `queued`.
//
// Idempotent prerequisites: the order must have StepsJSON populated
// (intake side stores it on creation) and be acquiring (pending or
// queued). Caller is responsible for the capacity gate — this method
// assumes green-light and proceeds with the atomic claim + dispatch.
//
// Called from:
//   - fulfillment.Scanner's one acquire for a door (complex intake, under
//     the door's scan lock)
//   - fulfillment.Scanner.tryFulfill on EventBinUpdated /
//     EventBinEnteredTransit / EventOrderCompleted etc. (slot vacancy
//     unblocks a previously-blocked order)
//
// A refusal it can wait out — no bin, no slot, a lane, a partner not yet
// received — parks the order acquiring with a queue reason and returns an
// error the scanner logs. What it cannot wait out — unparseable steps, a
// fleet rejection, a partner Core refused at intake — fails it through
// lifecycle.Fail.
func (d *Dispatcher) DispatchPreparedComplex(order *orders.Order) error {
	// Defense-in-depth: the fulfillment scanner already gates on IsAcquiring
	// ({pending, queued}) before calling here, so a parent in Reshuffling (with a
	// compound in flight), or one already dispatched or terminal, won't reach us
	// through the scanner. Anything calling this directly must still respect the
	// invariant — proceeding on a non-acquiring order would re-dispatch a parent
	// mid-reshuffle or race a post-resume. Return nil so the caller treats a
	// non-acquiring order as a no-op, not an error.
	if !protocol.IsAcquiring(order.Status) {
		d.dbg("complex: DispatchPreparedComplex called with status=%s (want pending/queued); skipping", order.Status)
		return nil
	}

	// ── THE PAIR IS THE UNIT OF WORK, WHEN THERE IS ONE (§ the pair rule) ──
	//
	// A coordinated multi-leg order dispatches both legs in this pass or neither.
	// The fork is on PAIR STRUCTURE — does this order name a sibling — and never
	// on SwapMode; see complex_pair.go for the rule and for why it is expressible
	// here, at dispatch, without touching a single line of release.
	//
	// A solo order falls straight through to the phases below, unchanged.
	legs, wait := d.coordinatedPairLegs(order)
	if wait != nil {
		if wait.refused != nil {
			return d.failForRefusedPartner(order, wait.refused)
		}
		return d.parkPairAwaitingPartner(order, wait.partner)
	}
	if len(legs) > 1 {
		return d.dispatchPairInOnePass(order, legs)
	}

	resolvedSteps, hand, _, st := d.prepareComplexHand(order, nil)
	if st.exchanged {
		// A claim exchange gave this order the mouth bin: acquire it once more
		// now, on the plan and hold the exchange wrote, rather than at the next
		// pass.
		if fresh, err := d.db.GetOrder(order.ID); err == nil && fresh != nil && protocol.IsAcquiring(fresh.Status) {
			*order = *fresh
			resolvedSteps, hand, _, st = d.prepareComplexHand(order, nil)
		}
	}
	if st.done {
		return st.err
	}
	return d.dispatchComplexToFleet(order, resolvedSteps, hand)
}

// prepareComplexHand is a complex order's acquisition, up to — and not including —
// anything hard: prepare, the swap gates, the destination's soft reserve, the
// sources' soft reserve, and the lane admission verdict. It returns the order's
// HAND — the slots and bins to confirm and the lane mouths to take — and the
// pickups it soft-reserved. A solo order's hand is committed by
// dispatchComplexToFleet, a pair's legs' together by commitPairToFleet, in one
// transaction with the occupancy and the status, and only then is the fleet
// asked. pc is the pair's pass (the leg prepared before this one), nil for a
// solo order.
//
// ── WHY NOTHING IS CONFIRMED HERE ─────────────────────────────────────────
//
// It used to be (acquireComplexPhases, the pair's stage 1 until C2b): the
// sources were reserved AND hard-confirmed, and the lanes were asked last, so a
// lane refusal was a wait taken holding every bin hard. The note that stood above the lane
// admit defended that on purpose — an order that dropped its claims to wait
// "would re-race for them every pass and could starve behind an order that
// arrived later." The owner has since ruled that a wait keeps SOFT reservations
// only (v4 §2.6), and the starvation argument is answered by the reservation
// itself: the soft holds survive the wait, and the hand's confirm is guarded by
// them — a hard claim may only land where this order already holds a live
// reservation — so a later order cannot take what this one is waiting to confirm.
//
// ── THE PASS THAT REACHES THE HAND RE-MAKES THE GROUP CHOICES ─────────────
//
// A queued order's saved group choices are re-made at acquiring (asIntent; the
// owner's direction), spelled as the pass that reaches the hand: the phases run
// first on the choices the order holds. A run that reaches the hand, or that a
// choice itself stopped (remakeAfter: a group with no room for it, an occupied
// group drop, a group pickup's bin gone), runs once more with the choices
// re-made, and that run's holds are what the hand takes: the allocator
// reconciles the order's soft holds against the plan it is handed
// (reserveComplexPlan), so a choice that moved moves its hold. A run stopped by
// anything else (material at a named node, a lane, the swap gates, a loader
// home) re-makes nothing and pays nothing for it. Re-making every pass under an
// unrelated cause cost 23 reads per queued order per pass (10 → 33, C2b item 7);
// this is +0 per queued order per pass, and about +27 once on the pass that
// dispatches a plan with group choices (the planner golden's
// complex-empty-pickup-press case, 95 → 122).
func (d *Dispatcher) prepareComplexHand(order *orders.Order, pc *pairPass) ([]resolvedStep, Hand, []reservedPickup, dispatchStep) {
	steps, hand, assigned, st, at := d.prepareComplexHandWith(order, pc, false)
	if !d.remakeAfter(order, at) {
		return steps, hand, assigned, st
	}
	steps, hand, assigned, st, _ = d.prepareComplexHandWith(order, pc, true)
	return steps, hand, assigned, st
}

// handPhase is where a run of the acquisition phases stopped.
type handPhase int

const (
	phasePrepare      handPhase = iota // stopped resolving a group choice (byChoice)
	phasePrepareOther                  // stopped in preparation for anything else
	phaseSwapGates
	phaseDestination
	phaseSources
	phaseHand
	phaseLanes
	phaseReached // the run built the hand
)

// remakeAfter says whether a run that stopped at phase at is followed by one that
// re-makes the plan's group choices. It is the choices' own business, and nothing
// else's: a run that reached the hand re-makes them before they are taken; one
// stopped by its destination re-makes them only if a drop is a group choice, one
// stopped by its sources only if a pickup is; one stopped in preparation if any
// step is. A run stopped by the swap gates, the hand or the lanes waits on
// something no group choice changes, and re-making there is the cost item 7
// counted. Read off the stored plan: a run stopped in preparation returns none.
func (d *Dispatcher) remakeAfter(order *orders.Order, at handPhase) bool {
	if at != phasePrepare && at != phaseDestination && at != phaseSources && at != phaseReached {
		return false
	}
	fresh, err := d.db.GetOrder(order.ID)
	if err != nil || fresh == nil || fresh.StepsJSON == "" {
		return false
	}
	var stored []resolvedStep
	if json.Unmarshal([]byte(fresh.StepsJSON), &stored) != nil {
		return false
	}
	intent := asIntent(stored)
	drop, pickup, reverted := false, false, false
	for i := range stored {
		if stored[i].Action == protocol.ActionDropoff && stored[i].Group != "" && stored[i].Node == stored[i].Group {
			reverted = true // the destination phase put an occupied drop back to its group
		}
		if intent[i].Node == stored[i].Node {
			continue
		}
		switch stored[i].Action {
		case protocol.ActionDropoff:
			drop = true
		case protocol.ActionPickup:
			pickup = true
		}
	}
	switch at {
	case phaseDestination:
		// A drop still naming its group here was reverted by this run (a
		// preparation that failed to resolve one stops in phasePrepare and is not
		// re-run: it would fail the same way, which is packed's ngrp-resolve
		// population at every pass).
		return drop || reverted
	case phaseSources:
		return pickup
	}
	return drop || pickup
}

// heldBinNodes is the set of nodes the order's soft-held bins stand on: the
// pickups asIntent keeps as chosen (asIntentKeeping). Read only on a run that
// re-makes; unreadable keeps nothing, which is asIntent's own answer.
func (d *Dispatcher) heldBinNodes(order *orders.Order) map[string]bool {
	rows, err := reservations.ListByOrder(d.db.DB, order.ID)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, h := range d.allocator.resolveHeldReservations(rows) {
		if h.kind == reservations.KindBin && h.nodeName != "" {
			out[h.nodeName] = true
		}
	}
	return out
}

// prepareComplexHandWith is one run of the acquisition phases; remake re-makes
// the plan's group choices first (asIntent). It reports where it stopped.
func (d *Dispatcher) prepareComplexHandWith(order *orders.Order, pc *pairPass, remake bool) ([]resolvedStep, Hand, []reservedPickup, dispatchStep, handPhase) {
	resolvedSteps, st := d.prepareComplexSteps(order, pc, remake)
	if st.done {
		if !st.byChoice {
			return nil, Hand{}, nil, st, phasePrepareOther
		}
		return nil, Hand{}, nil, st, phasePrepare
	}
	if st := d.applySwapGates(order, resolvedSteps); st.done {
		return nil, Hand{}, nil, st, phaseSwapGates
	}
	if st := d.reserveComplexDestination(order, resolvedSteps, pc); st.done {
		return nil, Hand{}, nil, st, phaseDestination
	}
	plan, assigned, st := d.reserveComplexSources(order, resolvedSteps)
	if st.done {
		return nil, Hand{}, nil, st, phaseSources
	}
	hand, st := d.handFromComplexPlan(order, plan, assigned)
	if st.done {
		return nil, Hand{}, nil, st, phaseHand
	}
	if st := d.admitComplexLanesVerdict(order, resolvedSteps); st.done {
		return nil, Hand{}, nil, st, phaseLanes
	}
	return resolvedSteps, hand, assigned, dispatchStep{}, phaseReached
}

// handFromComplexPlan turns a complete soft reserve into the hand that confirms
// it: every slot the plan drops into exclusively (with the bins its own earlier
// pickups take first), and every reserved pickup bin. It is confirmComplexPlan's
// walk with the confirms deferred to the hand, and without that function's
// "already ours, confirm in place" branches — they existed for a crash between
// the old per-leg transactions, which one transaction removes, and the claim SQL
// is owner-idempotent besides.
//
// The order's bin pointer and its order_bins rows are written here, before the
// hand, as confirmComplexPlan wrote them before the lanes were asked. They are
// the plan, not holds: a refused hand leaves them naming soft-held bins, which is
// the shape a lane-refused complex order already had, and the next pass rewrites
// them.
func (d *Dispatcher) handFromComplexPlan(order *orders.Order, plan *ComplexPlan, assigned []reservedPickup) (Hand, dispatchStep) {
	a := d.allocator
	steps := plan.ResolvedSteps
	claimFailed := func(detail string) (Hand, dispatchStep) {
		d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseClaimFailed,
			QueueParams{Payload: order.PayloadCode})
		d.dbg("complex: order %d held on claim_failed: %s", order.ID, detail)
		return Hand{}, dispatchStep{done: true, err: &planningError{Code: codeClaimFailed, Detail: detail}}
	}

	var hand Hand
	for _, sn := range a.slotNeeds(steps) {
		node, gerr := d.db.GetNodeByDotName(sn.nodeName)
		if gerr != nil || node == nil {
			return claimFailed(fmt.Sprintf("confirm slot %s for order %d: node unresolved", sn.nodeName, order.ID))
		}
		// A GATED GROUP DROP STAYS PAPER UNTIL RELEASE (C2c rulings 2 and 3; D13,
		// P0's premise correction 1). Its slot is chosen across the group again
		// when the robot stands at the group's spot, and the release hand takes
		// the chosen slot as armor then (DB.ConfirmReleaseDrop). Confirming the
		// pencil here is what made the plan's lane a commitment before anybody
		// asked whether it was the right one. A relay, a person-named slot and a
		// group without lanes keep today's moment (rulings 5, 6; Q-NAMED).
		if gatedGroupDrop(order, steps, sn.stepIndex, node) {
			continue
		}
		_, takenFirst, oerr := a.binsAtStep(order, steps, sn.stepIndex, sn.nodeName)
		if oerr != nil {
			return claimFailed(fmt.Sprintf("read slot %s for order %d: %v", sn.nodeName, order.ID, oerr))
		}
		slot := HandSlot{Node: node, TakenFirst: takenFirst}
		if st := partnerStamp(steps[sn.stepIndex]); st != nil {
			slot.Partner, slot.PartnerTaken = st.Partner, []int64{st.Bin}
		}
		hand.Slots = append(hand.Slots, slot)
		nodeID, name := node.ID, sn.nodeName
		hand.AfterCommit = append(hand.AfterCommit, func() {
			d.db.AppendAudit("node", nodeID, "slot_claimed", "",
				fmt.Sprintf("complex order %d slot confirm at %s", order.ID, name), "system")
		})
	}

	if len(assigned) == 0 {
		detail := fmt.Sprintf("complex order %d has no source pickup", order.ID)
		d.failOrderInternal(order, codeNoBin, detail)
		return Hand{}, dispatchStep{done: true, err: &planningError{Code: codeNoBin, Detail: detail}}
	}
	claimed := make([]claimedBin, 0, len(assigned))
	for _, rp := range assigned {
		hand.Bins = append(hand.Bins, HandBin{BinID: rp.binID})
		binID, name := rp.binID, rp.nodeName
		hand.AfterCommit = append(hand.AfterCommit, func() {
			d.db.AppendAudit("bin", binID, "claimed", "",
				fmt.Sprintf("complex order %d confirm at %s", order.ID, name), "system")
		})
		claimed = append(claimed, claimedBin{binID: rp.binID, stepIndex: rp.stepIndex, nodeName: rp.nodeName})
	}
	if err := a.recordClaimedBins(order, steps, claimed); err != nil {
		d.dbg("complex: order %d ended before its plan of bins was written: nothing held", order.ID)
		return Hand{}, dispatchStep{done: true, err: err}
	}
	return hand, dispatchStep{}
}

// admitComplexLanesVerdict is the lane question with no row written: the
// physical admission (and the dig it proposes for a buried pickup) and the
// tripwire on a plan that revisits a lane it leaves. A coordinated order takes no
// mouth hold: every lane is marked, and its entry is the gate's (refuseLaneRevisit).
func (d *Dispatcher) admitComplexLanesVerdict(order *orders.Order, resolvedSteps []resolvedStep) dispatchStep {
	// THE ADMISSION IS A READ HERE. skipsForComplexEntry declares the gate owns the
	// entry, and every lane is marked, so admitPlan admits every lane this plan
	// names and refuses nothing: the in-lane questions are asked at the tail
	// append. What it can still say is that a lane could not be read.
	if _, err := d.admitPlan(order, resolvedSteps, skipsForComplexEntry); err != nil {
		// FAIL CLOSED, with a cause that says so. An unreadable lane is a busy
		// lane; an undetermined answer is Core declining, not a lane that is
		// honestly occupied, and the two are investigated differently.
		log.Printf("dispatch: admission for complex order %d: %v (holding)", order.ID, err)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseAdmissionError,
			QueueParams{Destination: order.DeliveryNode})
		return dispatchStep{done: true, err: err}
	}

	hErr := d.refuseLaneRevisit(resolvedSteps)
	// ── ONE OF THESE ERRORS IS NOT A WAIT ─────────────────────────────────
	//
	// Everything else here fails closed into a hold, because an unreadable lane is
	// a busy lane and retrying is the right response. A LaneRevisitError is the
	// opposite kind of thing: the PLAN is malformed — it picks from a lane, leaves,
	// and comes back — and no amount of retrying changes a step list. Parking it
	// would hide a producer nobody knew existed behind a queue reason that says
	// "waiting for a slot", which is how a shape like this stays invisible.
	//
	// So it terminates, with `structural` (the code whose own doc is "malformed in
	// a way retrying cannot fix") and the tripwire's sentence as the detail, which
	// names the lane and both steps. It is deliberately loud: the shape has no
	// producer today, and the first one to appear should arrive as a failed order
	// somebody reads, not as a corridor quietly held for the life of an order.
	var revisit *LaneRevisitError
	if errors.As(hErr, &revisit) {
		log.Printf("dispatch: REFUSING complex order %d — %v", order.ID, revisit)
		d.failOrderInternal(order, codeStructural, revisit.Error())
		return dispatchStep{done: true, err: hErr}
	}
	if hErr != nil {
		log.Printf("dispatch: walking complex order %d's lanes: %v (holding)", order.ID, hErr)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseLaneAcquireError,
			QueueParams{Destination: order.DeliveryNode})
		return dispatchStep{done: true, err: hErr}
	}
	return dispatchStep{}
}

// prepareComplexSteps re-resolves and widens the order's stored steps (Phase A),
// persisting any changes, and returns the resolved slice for the later phases to
// read. It owns the re-resolution state machine: NGRP re-resolve (buried→replay /
// capacity→queue / other→fail), supply-pickup widening (found / wait / reshuffle /
// structural-fail), the persist-changed-steps + endpoint re-extract block, and the
// dedicated-loader placement. done=true means the order was parked or terminalized
// here and the orchestrator returns st.err verbatim; on done=false the returned
// slice is the single source of truth the read-only phases B–E consume.
func (d *Dispatcher) prepareComplexSteps(order *orders.Order, pc *pairPass, remake bool) ([]resolvedStep, dispatchStep) {
	var resolvedSteps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &resolvedSteps); err != nil {
		d.failOrderInternal(order, "invalid_steps", fmt.Sprintf("parse stored steps: %v", err))
		return nil, dispatchStep{done: true, err: err}
	}
	// A plan a refused create left spliced is worked as the plan it was spliced
	// from; the splice re-inserts its waits (withoutInboundLaneWaits).
	resolvedSteps = withoutInboundLaneWaits(resolvedSteps)
	// A queued order keeps its intent, not a choice made on an earlier pass: the
	// pass that reaches the hand makes every group choice again (asIntent;
	// prepareComplexHand says when).
	stored := resolvedSteps
	if remake {
		resolvedSteps = asIntentKeeping(resolvedSteps, d.heldBinNodes(order))
	}

	// Round-3 follow-up: re-resolve any step that still references an
	// NGRP. This happens on the deferred path — intake queued the order
	// because the NGRP was saturated; the scanner replays after slot
	// vacancy events, and we attempt resolution again here. On capacity
	// failure, set queue_reason to the current resolver message and
	// stay queued (don't fail). On other resolver errors, fail with
	// invalid_steps. On success, persist the locked-in concrete-child
	// names so subsequent ticks don't redo the work.
	// THE RESUMING PARENT IS THE CASE THE ASKER EXISTS FOR. After an expose dig
	// the lane lock is held BY THIS ORDER to protect the bin it uncovered; an
	// owner-blind re-resolve drops that lane and sends the parent back to a
	// buried bin it will dig for again. See store/reservations/dig_exclusion.go.
	newSteps, _, rerr := d.reResolveComplexStepsFor(order, resolvedSteps, digAskerFor(order), pc)
	if rerr != nil {
		class, payload := classifyResolutionError(rerr)
		switch class {
		case ResolutionBuried:
			// Multi-burial scenario: a second-or-later step in the
			// order hit a burial after the first compound completed.
			// Same planner the intake path uses.
			buriedErr := payload.(*BuriedError)
			d.dbg("complex: order %d buried at replay — bin %d in lane %d", order.ID, buriedErr.Bin.ID, buriedErr.LaneID)
			swapped := d.handleComplexBuriedOnReplay(order, buriedErr)
			return nil, dispatchStep{done: true, err: rerr, exchanged: swapped}
		case ResolutionCapacity:
			capDetail := capacityDetailFrom(payload)
			code := queueCodeForCapacity(capDetail.kindOf())
			d.setQueueReason(order, code, causeForCapacity(capDetail.kindOf(), CauseNGRPResolve),
				queueParamsForCapacity(capDetail, order.PayloadCode, order.DeliveryNode))
			d.dbg("complex: order %d still capacity-blocked at NGRP resolution: %s", order.ID, code)
			return nil, dispatchStep{done: true, err: rerr, byChoice: true}
		default:
			d.failOrderInternal(order, "invalid_steps", rerr.Error())
			return nil, dispatchStep{done: true, err: rerr}
		}
	}

	// C(ii): supply-pickup widening. Every full-material pickup — never an
	// evac/removal leg (isRemovalPickup splits those off; they keep today's
	// reserve/moot path byte-unchanged) — is re-anchored through the
	// node-local finder each tick: material sitting on a sibling position in
	// the anchor's pool rewrites the step there; a dry anchor PARKS the order
	// as waiting_for_material instead of letting it fail terminal downstream.
	// The park is the disposition change: a dry supply need is escapable
	// (operator abandon) rather than a cancel→autoreorder loop.
	widened, _, hold := d.widenSupplyPickups(order, newSteps)
	newSteps = widened
	// PERSISTED WHEN THE PLAN DIFFERS FROM THE ONE STORED, not when a stage says
	// it moved something: asIntent puts every group choice back, so the stages
	// report a change on every pass that re-chooses the same slot. Comparing the
	// result with what was loaded writes only a choice that actually changed.
	changed := !reflect.DeepEqual(newSteps, stored)

	if changed {
		d.persistWidenedPlan(order, newSteps)
	}
	resolvedSteps = newSteps

	// A blocked supply need surfaces AFTER the persist block so partial
	// rewrites (steps widened before the blocked one) are already durable;
	// the next tick re-derives every anchor from the persisted Group stamp.
	if hold != nil {
		switch MapFinderOutcome(*hold) {
		case OutcomeWait:
			d.setQueueReason(order, hold.QueueCode, hold.QueueCause, hold.QueueParams)
			d.dbg("complex: order %d supply pickup waiting for material (%s)", order.ID, hold.QueueCause)
			return nil, dispatchStep{done: true, err: fmt.Errorf("supply pickup waiting for material: %s", hold.QueueCause)}
		case OutcomeReshuffle:
			d.dbg("complex: order %d supply pickup buried at widen — bin %d in lane %d",
				order.ID, hold.Buried.Bin.ID, hold.Buried.LaneID)
			swapped := d.handleComplexBuriedOnReplay(order, hold.Buried)
			return nil, dispatchStep{done: true, err: fmt.Errorf("supply pickup buried; reshuffle planned"), exchanged: swapped}
		case OutcomeFound:
			// Impossible: widenSupplyPickups only holds non-Found results.
			// Fail structurally rather than silently dispatching a plan whose
			// widening never finished.
			fallthrough
		default: // OutcomeStructural — TermCode/Err may be unset on the
			// loud-degrade path out of MapFinderOutcome.
			code := hold.TermCode
			if code == "" {
				code = codeNoBin
			}
			detail := "supply widening failed structurally"
			if hold.Err != nil {
				detail = hold.Err.Error()
			}
			d.failOrderInternal(order, code, detail)
			return nil, dispatchStep{done: true, err: fmt.Errorf("supply pickup structural: %s", detail)}
		}
	}

	// Dedicated home loader PARK: when this is a changeover return from a
	// dedicated-loader home (order.SourceNode = the evac pickup), Core decides where
	// the bin lands — HOME if free, else a buffer slot — and rewrites DeliveryNode.
	// The Edge shipped DeliveryNode="" and named no target, so Core is the single
	// authority; the release-time redirect overlay (patchRedirectSegments) carries
	// the choice to the fleet. A non-dedicated / non-loader source is left untouched.
	//
	// When neither can take the bin and the only destination left is the home just
	// found unavailable, the order WAITS here rather than being dispatched onto it.
	// Only a leg whose drop is a loader home can reach that answer, so a supply leg
	// bound for the line is never held by it.
	if waitHome := d.placeForDedicatedLoader(order, resolvedSteps, pc); waitHome != "" {
		if d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseLoaderParkNoSlot,
			d.parkNoSlotParams(order, resolvedSteps, waitHome)) {
			log.Printf("dispatch: complex order %d waits — loader home %s cannot take its bin and no buffer is free",
				order.ID, waitHome)
		}
		return nil, dispatchStep{done: true, err: fmt.Errorf("loader home %s unavailable and no buffer free", waitHome)}
	}

	// Quality containment: an FG-bound leg of a contained payload re-points to
	// its claim's containment destination (or parks, containment full). Runs
	// AFTER the dedicated-loader park so both authorities agree on the plan
	// this tick; a non-nil step means the order was parked and the orchestrator
	// returns it verbatim.
	if st := d.placeForContainment(order, resolvedSteps); st != nil {
		return nil, *st
	}

	return resolvedSteps, dispatchStep{}
}

// reserveComplexSources is the sources' SOFT half, shared by the solo order and
// the pair: the plan, and a pending reservation on every
// pickup bin. Nothing is confirmed here: a solo order confirms in its hand, a
// pair's legs in their one hand.
func (d *Dispatcher) reserveComplexSources(order *orders.Order, resolvedSteps []resolvedStep) (*ComplexPlan, []reservedPickup, dispatchStep) {
	// THE GUARD AGAINST A CANCELLED ORDER is in the reservation statements: every
	// insert takes the owner's row and refuses a terminal one
	// (reservations.OwnerLiveSQL), and the refusal stops this pass (ownerEnded).
	// The hand's status CAS (acquireHand) is the last one before a robot moves.
	// Acquiring writes no status of its own (C1: acquiring is an act, not a rung).
	// Plan = the resolved steps. The reserve selects against live state from the
	// steps alone and reads the carrier rule itself (reserveComplexPlan).
	plan := BuildComplexPlan(resolvedSteps)

	// Reserve = reconcile held reservations against the distinct source needs and
	// soft-hold the gaps (reserveComplexPlan). Runs AFTER the slot-claim loop above,
	// never interleaved with it (slots-before-bins) — one claim class fully ordered
	// before the next is what prevents a slot↔bin cross-type deadlock cycle. Dispatch
	// is gated on a COMPLETE distinct-bin set (the relay rule): an incomplete order
	// holds its partials and parks for the scanner to retry — a robot never starts a
	// job it can't finish, and give-up is operator-driven, never a timer. A crash
	// leaves a pre-dispatch order whose pending holds the owner-liveness reaper
	// reclaims, never one with claimed bins: the hard claims are the hand's.
	assigned, outcome, rerr := d.allocator.reserveComplexPlan(order, plan)
	if rerr == nil && outcome == reserveComplete {
		d.placeByContentsAtReserve(order, plan, assigned)
	}
	if plan.Reanchored {
		d.persistWidenedPlan(order, plan.ResolvedSteps)
	}
	if ownerEnded(rerr) {
		d.dbg("complex: order %d ended before its source reserve: nothing held", order.ID)
		return nil, nil, dispatchStep{done: true, err: rerr}
	}
	if rerr != nil {
		// A DATABASE ERROR, AND THE ROW SAYS SO. This arm parked the order and
		// wrote nothing, so an order stuck behind a database that
		// was not answering looked exactly like one nobody had reached yet — the
		// blank-cause residue the liveness floor kept reporting as "(none)".
		// reserveComplexPlan's errors are all reads and reservation writes; they
		// clear on their own, and the scanner replays.
		log.Printf("dispatch: complex order %d reserve error: %v", order.ID, rerr)
		d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseReadFailed,
			QueueParams{Payload: order.PayloadCode})
		return nil, nil, dispatchStep{done: true, err: rerr}
	}
	switch outcome {
	case reserveMoot:
		// Reserved nothing and every source node is empty — the work is void (e.g. a
		// swap evac whose line bin was removed to quality hold before dispatch). Skip
		// so Edge's HandleOrderSkipped advances the linked changeover task, rather
		// than hold forever: a moot evac is not demand (operator-driven hold-and-retry
		// does not apply).
		d.skipOrderInternal(order, codeNoSourceBin, fmt.Sprintf("complex order %d: no bin at any source node", order.ID))
		return nil, nil, dispatchStep{done: true, err: fmt.Errorf("complex order %d moot — skipped", order.ID)}
	case reserveHolding:
		// Only claim "partial set already held" when the order actually holds part
		// of its set. Holding NOTHING (zero reserved, blocked on every need) is a
		// different operator situation and must not render as a partial hold — that
		// was the SPR ALN_006 lie: "partial set already held" while the
		// order held zero reservations and made no progress.
		holdingPartials := len(assigned) > 0
		d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseReserveHolding,
			QueueParams{Payload: order.PayloadCode, Partial: holdingPartials})
		d.dbg("complex: order %d incomplete reserve — holding %d partial(s), retrying next tick", order.ID, len(assigned))
		return nil, nil, dispatchStep{done: true, err: fmt.Errorf("complex order %d reserve incomplete", order.ID)}
	case reserveHoldingForOutside:
		// The pair holds for the carrier at a position its cell depends on:
		// coming there, or named by a move taking it away. parkPair copies this
		// wait to the partner whole, so both legs read waiting_for_outside.
		d.setQueueReason(order, protocol.QueueWaitingForOutside, CauseHoldingForOutside,
			QueueParams{Payload: order.PayloadCode})
		d.dbg("complex: order %d holds for outside material, retrying next tick", order.ID)
		return nil, nil, dispatchStep{done: true, err: fmt.Errorf("complex order %d holds for outside material", order.ID)}
	}

	return plan, assigned, dispatchStep{}
}

// ownerEnded reports a reservation refused because its order has ended: a
// cancel landed after the caller read the order. The caller stops — nothing is
// held, nothing is written on the row, nothing is retried — and says so only at
// debug.
func ownerEnded(err error) bool { return errors.Is(err, reservations.ErrOwnerEnded) }

// complexCommit is one complex order's create, built and not yet sent: the
// request, the nodes its first segment enters (whose lanes' occupancy the hand
// takes), the hand, and what to do once the fleet has it. A solo order commits
// one (dispatchComplexToFleet); a pair commits both legs' in one hand
// (commitPairToFleet).
type complexCommit struct {
	order    *orders.Order
	steps    []resolvedStep // the plan the create was built from (spliced when gated)
	req      fleet.CreateOrderRequest
	entering []*nodes.Node
	hand     Hand
	actor    string
	told     QueueWait // the wait the station was last told, before the hand
	gated    bool
	target   laneGateTarget
	hasWait  bool
}

// dispatchComplexToFleet is the solo complex order's tail (Phase E): build the
// create, take the hand and send it (commitToFleet), then mark it complete or
// leave it staged, clear its wait and emit. A build that does not produce a
// create has already disposed of the order (buildComplexCommit); a handover
// that does not send it goes through refuseComplexHandover.
func (d *Dispatcher) dispatchComplexToFleet(order *orders.Order, resolvedSteps []resolvedStep, hand Hand) error {
	c, err := d.buildComplexCommit(order, resolvedSteps, hand, WaitOf(order))
	if err != nil {
		return err
	}
	// RECORD THE PRESENCE, then claim, commit and name it — commitToFleet
	// (fleet_handover.go) is the seam every arm goes through.
	//
	// THE ROW USED TO BE MISSING HERE, and this was the one arm it was missing
	// from. A complex order asked admission "is anyone inside this lane" and
	// waited when the answer was yes — it respects the gate as an ENTRANT — and
	// then dispatched without ever appearing in anyone else's answer. It skipped
	// the reciprocal duty: making its own presence visible so the gate can protect
	// everyone else FROM it.
	//
	// The collision that allowed has no illegal step in it. A plain store or a dig
	// leg asks the question, the ledger says the lane is empty because nobody
	// wrote the page, and the admission that follows is LAWFUL. Two robots, one
	// single-file lane, clean paperwork throughout — which is why no checker saw
	// it: every runtime and soak assertion about "who is inside" reads the same
	// rows this arm was not writing.
	//
	// It is the arm that mattered most. Complex is the bulk of both plants' lane
	// traffic, and THIS branch — ungated — is the one that runs, because no lane
	// at either plant carries a mark.
	//
	// THE NODES ARE THE PRE-WAIT SEGMENT'S, not the whole plan's, and the
	// distinction is the same one the create makes: only `preWait` is being
	// dispatched now. Steps after the wait are a segment this robot has not been
	// sent on yet, and taking a row for a lane it may reach in ten minutes would
	// wall that lane for the whole dwell — the mistake the gated arm exists to
	// avoid, arrived at from the other direction.
	if err := d.commitToFleet(order, c.req, c.actor, hand, c.entering...); err != nil {
		d.refuseComplexHandover(order, c.told, err)
		return err
	}
	d.afterComplexCreate(c)
	return nil
}

// buildComplexCommit builds a complex order's create: the lane wait spliced in,
// the junction re-indexed onto the spliced plan, and the request built from the
// segment before the first wait. Nothing is held and nothing is sent. On an
// error the order has been disposed of here — failed for a plan Core cannot
// send, waiting for a database that did not answer, or, for the gated valve's
// own refusals, through refuseComplexHandover — and the caller only returns it.
// told is the wait the station was last told, carried for the refusal door.
func (d *Dispatcher) buildComplexCommit(order *orders.Order, resolvedSteps []resolvedStep, hand Hand, told QueueWait) (*complexCommit, error) {
	// THE SPLICE, AND THIS IS WHERE IT LIVES FOR COORDINATED ORDERS.
	//
	// A coordinated order never reaches dispatchToFleetCore — the scanner branches
	// on IsCoordinated to DispatchPreparedComplex, and this is its fleet create. So
	// the two dispatcher conditions that used to exclude coordinated orders from
	// the valve were inert, and deleting them was tidying: the transform had to be
	// installed HERE to reach this population at all.
	//
	// BEFORE splitAtWait, so the inserted wait is part of the split the create is
	// built from — everything up to the lane goes out now, and the rest becomes the
	// tail. An order does all the work it can before it dwells.
	//
	// RUNS ONCE PER CREATE. A successful dispatch leaves the order `dispatched`,
	// so this phase cannot re-enter for it. A create the fleet refused re-queues
	// the order with the spliced plan persisted (Q6); its next pass strips the
	// waits at load (withoutInboundLaneWaits), so this splices once again.
	spliced, target, gated, err := d.spliceLaneWait(resolvedSteps, order.ID)
	if err != nil {
		// A refusal here is structural: a lane entry that is not concrete yet, or a
		// wait that would gate nothing. Both are plans Core cannot gate safely, and
		// shipping them ungated is the failure the gate exists to prevent.
		//
		// TWO GATED LANES USED TO LAND HERE AND NO LONGER DOES. A swap picking in
		// one marked lane and dropping in another is a legitimately shaped request,
		// and failing it terminated the demand for a shape the plant is allowed to
		// have — the wait-not-fail law, broken by the one arm that was supposed to
		// enforce it. It now gets a wait at each lane's mark, released
		// independently by each lane's own admission.
		d.failOrderInternal(order, "invalid_steps", err.Error())
		return nil, err
	}
	// The splice moved the plan; order_bins still points at the old positions.
	// Repair before anything reads the junction against the spliced plan — the
	// lane gate is the first such reader and it is one dispatch away.
	//
	// A FAILURE HERE IS A DATABASE ERROR, SO IT WAITS. Every error this can
	// return is one — a failed read of order_bins, or a failed UPDATE inside the
	// shift transaction. It used to call failOrderInternal(order,
	// "invalid_steps"), which is wrong twice: it TERMINATES demand for
	// congestion, which is the one thing wait-not-fail forbids (F-04's shape, one
	// commit old), and it terminates it under a label that sends the next reader
	// to the planner to debug a plan that is perfectly well-formed.
	//
	// The order stays in its entry status and the scanner replays it — the same
	// releaser every acquiring wait rides, and the same cause the class already
	// uses for "the database did not answer" (compound.go's two node reads,
	// complex_reshuffle.go's).
	if rErr := d.reindexOrderBinsForSplice(order.ID, spliced); rErr != nil {
		log.Printf("dispatch: complex order %d — could not re-index its junction onto the spliced "+
			"plan: %v (holding; the scanner replays it)", order.ID, rErr)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{})
		return nil, rErr
	}
	if gated {
		// One valve, shared with the plain path. nil load sequence: F4c is scoped
		// to the simple transport path and complex has never been expanded.
		req, preWait, gErr := d.buildGatedCreate(order, target, spliced, nil)
		if gErr != nil {
			d.refuseComplexHandover(order, told, gErr)
			return nil, gErr
		}
		return &complexCommit{order: order, steps: spliced, req: req, entering: d.planNodes(preWait),
			hand: hand, actor: "dispatcher", told: told, gated: true, target: target, hasWait: true}, nil
	}

	preWait, hasWait := splitAtWait(resolvedSteps)
	vendorOrderID := mintVendorOrderID(order.ID)
	// Complex orders are not load-sequence expanded (nil): the F4c advanced load
	// sequence is scoped to the simple transport path the child-cart delivery
	// uses. Complex is "every other order kind" — byte-identical to before.
	blocks := stepsToBlocks(vendorOrderID, preWait, 0, nil)
	if len(blocks) == 0 {
		d.failOrderInternal(order, "invalid_steps", "no actionable steps before wait")
		return nil, fmt.Errorf("no actionable blocks")
	}

	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   order.Priority,
		RobotGroup: d.robotGroupForOrder(order),
		Vehicle:    pinnedVehicleFor(order),
		// The claim's routing hints, if it configured any. Nil/empty is SEER
		// auto-pick, which is every order in the plant until one does.
		KeyRoute: order.KeyRoute,
		KeyTask:  order.KeyTask,
		Complete: false, // staged: a multi-wait complex order dwells (Complete=false) until its final segment is released
	}
	d.dbg("complex: creating staged order %s with %d initial blocks (hasWait=%v)", vendorOrderID, len(blocks), hasWait)
	return &complexCommit{order: order, steps: resolvedSteps, req: req, entering: d.planNodes(preWait),
		hand: hand, actor: "scanner", told: told, hasWait: hasWait}, nil
}

// afterComplexCreate is what follows a create the fleet took: a gated order's
// valve half (its tail appended now if the lane is safe), or an ungated order
// with no wait marked complete; then the wait cleared and the event emitted.
func (d *Dispatcher) afterComplexCreate(c *complexCommit) {
	order := c.order
	if c.gated {
		// Cleared BEFORE the valve: the create ended the wait the order was told,
		// and a valve that refuses entry writes its own (F2). Clearing after it
		// wiped that refusal, so a complex order held at the mark said nothing.
		d.setQueueReason(order, "", "", QueueParams{})
		d.afterGatedCreate(order, c.target, c.steps, c.req.OrderID)
		log.Printf("dispatch: complex order %d dispatched gated into lane %s (%d steps)",
			order.ID, c.target.lane.Name, len(c.steps))
		return
	}
	if !c.hasWait {
		// No wait — fleet can complete the order immediately.
		//
		// This now runs AFTER the id write rather than between the create and it.
		// The gap it moves across is two database statements, and nothing reads
		// the vendor's completion flag in between; the create is Complete:false
		// either way, so the fleet cannot finish the order early in that window.
		if err := d.backend.ReleaseOrder(c.req.OrderID, nil, true); err != nil {
			log.Printf("dispatch: fleet mark complete failed: %v", err)
		}
	}

	log.Printf("dispatch: complex order %d dispatched as %s (%d steps)", order.ID, c.req.OrderID, len(c.steps))
	// Successful dispatch — clear any stale queue_reason from a prior
	// blocked replay attempt.
	d.setQueueReason(order, "", "", QueueParams{})
	d.emitter.EmitOrderDispatched(order.ID, c.req.OrderID, order.SourceNode, order.DeliveryNode)
}

// applySwapGates runs the one coordinated-swap guard left at dispatch: the swap
// peer-terminal race unwind (SPR 2424/2425) that resolves a leg whose sibling
// already went terminal. The name is older than that; the note at the end of
// the function lists the three gates that stood here and why each went.
//
// THE REMOVAL-LEG HOLD IS NOT HERE ANY MORE. This used to run Face 1 — park the
// evac until its supply secured a claim, "to prevent stranding" (ALN_003,
// 2026-06-03) — and that arm is deleted. Dispatch sends an evac to PARK, so the
// hold guarded a step that moves nothing, and the anti-strand is now structural:
// complex_pair.go dispatches both legs of a pair in one pass or neither. The
// index anti-collision hold (HOP 07) went the same way.
//
// done=true means the order was resolved by the unwind; the orchestrator
// returns st.err verbatim. It takes the resolved steps for the phase signature
// and reads none of them.
func (d *Dispatcher) applySwapGates(order *orders.Order, resolvedSteps []resolvedStep) dispatchStep {
	// Close the swap peer-terminal RACE (SPR 2424/2425, 2026-07). HandleSwapPeerTerminal
	// unwinds a swap when one leg reaches a terminal state, but it fires from the
	// DEAD leg's side — so if this leg did not exist yet when its sibling died (a
	// supply created + skipped moot in the same tick, before its evac was created),
	// that unwind found no peer and no-op'd, leaving this leg to hold forever on a
	// dead sibling. Re-run the
	// unwind now, from the surviving side, so a leg linked to an already-terminal
	// sibling is resolved instead of wedged. Reuses the same handler and its
	// per-role resolution: a moot-evac sibling that legitimately lets this supply
	// proceed is a no-op there, so this leg falls through and dispatches.
	if sibUUID, serr := d.db.OrderSiblingUUID(order.ID); serr == nil && sibUUID != "" {
		if sib, gerr := d.db.GetOrderByUUID(sibUUID); gerr == nil && sib != nil && protocol.IsTerminal(sib.Status) {
			if kind := swapTerminalKind(sib.Status); kind != "" {
				// Heal the dead leg's back-link first (idempotent). The unwind
				// resolves the peer FROM the dead leg's side, and the race is
				// precisely that this link may not have existed when the dead leg
				// first went terminal — so ensure it does now.
				if sib.SiblingOrderUUID != order.EdgeUUID {
					if _, rerr := d.db.LinkOrderSiblingsByEdgeUUID(order.EdgeUUID, sibUUID); rerr != nil {
						log.Printf("dispatch: swap race back-link repair order %d sib %s: %v", order.ID, sibUUID, rerr)
					}
				}
				d.HandleSwapPeerTerminal(sib.ID, kind)
				// RE-READ, BECAUSE THE UNWIND MAY HAVE JUST CANCELLED THIS ROW.
				// HandleSwapPeerTerminal writes to the very order we are
				// dispatching, so the in-memory copy the scanner handed us is
				// stale from here on and proceeding on it would dispatch a robot
				// for an order that no longer exists.
				//
				// IT USED TO CARRY THE FRESH ROW FORWARD (`order = self`) as well,
				// because the swap-hold verdict below judged it and a leg spared on
				// THIS pass still looked unspared. Both the spare and that verdict
				// are deleted, so nothing downstream reads the row again inside
				// this function — the assignment became dead and staticcheck said
				// so. The TERMINALITY TEST is the part that was always load-bearing
				// and it stays.
				if self, rerr := d.db.GetOrder(order.ID); rerr == nil && self != nil &&
					protocol.IsTerminal(self.Status) {
					return dispatchStep{done: true, err: fmt.Errorf("complex order %d resolved by swap peer-terminal unwind: sibling %d already %s", order.ID, sib.ID, sib.Status)}
				}
			}
		}
	}

	// ── AND NOTHING ELSE. THERE IS NO SWAP HOLD AT DISPATCH ANY MORE ──────
	//
	// Three gates stood here across this batch's life and all three are gone,
	// for one reason: DISPATCH DOES NOT MOVE MATERIAL FOR A SWAP LEG. Every leg
	// of a coordinated swap opens with a WAIT, so what dispatch sends a robot is
	// "drive to your node and hold" — splitAtWait returns the plan up to that
	// wait and the rest is appended at RELEASE.
	//
	//	Face 1  held an evac until its supply had claimed, "to prevent
	//	        stranding". It was guarding a parked robot.
	//	Face 3  held a supply while its evac was blocked on capacity. Buried
	//	        2026-08-31; its same-resource exemption emptied it.
	//	Face 2  held a filler until its clearer had committed, so no bin would be
	//	        placed on an un-cleared position. Also guarding a parked robot:
	//	        which robot parks first is not a fact about anything, because
	//	        neither touches a carrier until the operator releases.
	//
	// WHAT REPLACES THEM IS NOT ANOTHER GATE. Both legs are admitted in one
	// scanner pass or neither is (complex_pair.go), so a parked evac implies a
	// dispatched supply by construction; a leg going terminal takes its sibling
	// (swap_peer.go); and the collision hazard is answered at RELEASE, where the
	// bins actually move and the Edge's press-index lift rule (release.PlanAct's G7) already orders
	// the legs. That guard is not this layer's to help.
	//
	// The peer-terminal unwind above stays. It resolves a leg whose sibling was
	// ALREADY dead when this pass began, which is a question about the pair's
	// existence rather than about sequencing two live robots.
	return dispatchStep{}
}

// reserveComplexDestination runs the destination-side gates (Phase C): the
// dropoff-capacity gate for concrete STORAGE slots and DECLARED-exclusive
// dropoffs (regression 2b05dce) followed by the reservation-native slot reserve
// (the split-brain fix). done=true means the order parked waiting_for_slot
// (capacity-blocked or incomplete reserve) or hit a reserve error; the
// orchestrator returns st.err verbatim. Runs before the bin reserve
// (slots-before-bins). Reads the resolved steps read-only.
func (d *Dispatcher) reserveComplexDestination(order *orders.Order, resolvedSteps []resolvedStep, pc *pairPass) dispatchStep {
	// #1 (regression 2b05dce): restore the dropoff-capacity gate for complex
	// orders, but ONLY for concrete STORAGE dropoffs. The scanner dropped the
	// gate for every complex order to unstick two-robot SUPPLY legs — which
	// deliver to a LINE node a sibling EVAC clears — but that also let a
	// changeover drop/evac to a FULL concrete storage slot dispatch into the
	// occupied slot. Gate by node role (a storage dropoff, storageDropoffOf: a
	// STOR node or a child of a LANE or an NGRP, one rule with the plain path's
	// since 2026-10-25), NOT by same-order pickup: gating the line case would
	// re-create the deadlock 2b05dce fixed. NGRP dropoffs are already covered above by
	// reResolveComplexStepsFor / ResolutionCapacity. Stay queued by returning an
	// error — the scanner keeps the order queued and replays it on the next
	// slot-vacancy tick (same contract as the claim_failed branch below).
	//
	// THIS USED TO SAY "STORAGE/STAGING", AND IT NEVER COVERED STAGING — see
	// storageDropoffOf's own note. Staging is handled by the declared
	// loop further down instead.
	//
	// THE PROHIBITION STANDS; ITS OLD REASON DOES NOT. The justification used to
	// end "and Core has no SiblingOrderID to model that". Core has it:
	// orders.sibling_order_uuid is stamped at intake (complex_intake.go), linked
	// durably by LinkOrderSiblingsByEdgeUUID, and read by the pair rule
	// (complex_pair.go coordinatedPairLegs) and the death rule (swap_peer.go).
	// (It also named `sibling_order_id`, which is EDGE's column — an
	// INTEGER FK in SQLite. Core's is sibling_order_uuid, TEXT, holding the peer's
	// edge UUID. The two services genuinely differ here and the error travelled as
	// far as a round prompt.) That obstacle was removed some time ago; the history
	// is kept because somebody will re-derive it otherwise.
	//
	// WHAT ACTUALLY KEEPS THIS A BLUNT ROLE TEST IS THE PHASE IT RUNS IN, and
	// modelling the sibling does not move that. This gate runs BEFORE the sources
	// are acquired, so a refusal here holds CLAIMING, not fleet-create. Hold a
	// supply on the line node its evac is on the way to clear and you have not
	// sequenced the pair — you have two legs each waiting for the other to acquire,
	// which is a mutual wait no event ends. That is the 2b05dce deadlock, and it
	// is a property of WHERE the check sits rather than of how well the dependency
	// is expressed: a perfectly modelled dependency evaluated at this phase
	// deadlocks exactly the same way.
	//
	// So the rule is unchanged and the reason is now the one that holds. Moving
	// the check to a later phase is the design change that would reopen it, and
	// that is a different conversation from adding a sibling link.
	// finalChecked records whether the DeliveryNode arm below actually ran, so the
	// declared-dropoff loop knows whether that node still needs asking about.
	//
	// IT IS NOT `s.Node == order.DeliveryNode`, and the difference is a hole. An
	// order whose FINAL dropoff is itself a staging node — BuildStageSteps is
	// exactly that shape, pickup source then dropoff staging, nothing after —
	// is no storage dropoff, so this arm skips it; and a loop that then
	// skipped it again for equalling DeliveryNode would leave it checked by
	// nobody. The two arms have to compose on what was ASKED, not on which node
	// it was.
	finalChecked := isStorageDropoff(d.db, order.DeliveryNode)
	if finalChecked {
		blocked, cap := CheckDropoffCapacity(d.db, order.DeliveryNode, order.ID)
		// ── THE VACATED-SLOT RULE, IN TODAY'S WAIT BRANCH ONLY ────────────────
		//
		// Three cases, in this order, when the final drop is blocked:
		//
		//   1. A drop already stamped by the arm that chose it (the NGRP or loader
		//      vacated arm, or this arm on an earlier pass) is re-asked, and passes
		//      while the rule still holds. Without this the revert below would send
		//      it back to its group every pass and the market case would loop.
		//   2. An UNSTAMPED FUNGIBLE drop reverts to its group exactly as before,
		//      and the rule is not asked, so a free child still wins.
		//   3. An UNSTAMPED NON-FUNGIBLE drop asks the rule where it would have
		//      waited, and is stamped if admitted — the NGRP-child home case.
		//
		// Only an occupied node is the rule's question. A node an order is already
		// inbound to has nothing on it to vacate.
		if blocked && cap.Cause == CauseDropoffOccupied {
			if f := lastDropIndex(resolvedSteps); f >= 0 && resolvedSteps[f].Node == order.DeliveryNode {
				fungible := resolvedSteps[f].Group != "" && resolvedSteps[f].Group != resolvedSteps[f].Node
				if plan.StampHonoured(resolvedSteps[f]) != nil || !fungible {
					if st := d.vacatedFor(order, resolvedSteps, f, order.DeliveryNode, pc); st != nil &&
						d.noOtherInFlight(order.DeliveryNode, order) {
						if prev := resolvedSteps[f].Vacate; !plan.SameGrant(prev, st) {
							d.admitVacated(order, resolvedSteps, f, st, "final-drop gate")
						}
						blocked = false
					}
				}
			}
		}
		if blocked {
			// ── A FUNGIBLE DROPOFF RE-ASKS RATHER THAN WAITING ON ONE SLOT ────
			//
			// The step remembers the group it was resolved from, and resolvedStep.Group
			// says why in as many words: "Retained so a store drop-off that loses the
			// dispatch-time slot claim can revert Node->Group and be re-resolved to a
			// free slot on the next scanner tick." The allocator already does exactly
			// that when a slot RESERVATION conflicts (allocator.go, the escape valve).
			// This arm — the CAPACITY block — did not, and the difference is the whole
			// defect: an occupied slot parked the order on that one slot forever while
			// its siblings stood empty.
			//
			// GATED SIM 2026-08-31, order 227. A CLIP evac out of ALN_006 was bound to
			// SMN_015, which held an unrelated STUD bin. SMN_013 and SMN_014, the same
			// lane, were EMPTY the whole time. The order re-queued 4,520 times — 96% of
			// every dropoff-occupied event in the run — and never re-asked. Worse than
			// the spin: it was the evac for ALN_006, so the cell kept the bin nobody was
			// coming for, and the supply leg (order 225) stood HOLDING at that cell
			// unable to place. One un-re-asked slot pinned two orders and a machine.
			//
			// Reverting the node to its group is not a new destination-picking path; it
			// is handing the question back to the one that already exists. If the whole
			// group is genuinely full, reResolveComplexStepsFor parks the order on
			// ngrp-resolve next tick — a wait whose releaser is "any slot in the group
			// frees", which is true, instead of "this one slot frees", which was not.
			if reverted := d.revertFungibleDropoffToGroup(order, resolvedSteps); reverted {
				d.setQueueReason(order, protocol.QueueWaitingForSlot, cap.Cause, cap.Params)
				d.dbg("complex: order %d dropoff %s blocked (%s) — reverted to its group so the next "+
					"tick re-resolves", order.ID, order.DeliveryNode, cap.Cause)
				return dispatchStep{done: true, err: fmt.Errorf("dropoff capacity: %s (re-asking the group)", cap.Cause)}
			}
			// cap.Cause, not the coarse tag — see CauseDropoffCapacity's own note.
			d.setQueueReason(order, protocol.QueueWaitingForSlot, cap.Cause, cap.Params)
			d.dbg("complex: order %d queued — concrete storage dropoff %s blocked: %s", order.ID, order.DeliveryNode, cap.Cause)
			return dispatchStep{done: true, err: fmt.Errorf("dropoff capacity: %s", cap.Cause)}
		}
	}

	// ── AND THE DECLARED-EXCLUSIVE DROPOFFS, WHICH THE ARM ABOVE CANNOT SEE ───
	//
	// That arm asks about the FINAL destination only. A stage-and-accept plan puts
	// a bin down twice — `dropoff SLN_003 / wait / pickup SLN_003 / dropoff
	// ALN_004` — and the staging stop is not order.DeliveryNode, so no capacity
	// question was ever asked about it. That is how a robot reached a full
	// SLN_003 and stood there for 48 minutes (Springfield, 2026-08-12).
	//
	// WHY BOTH THIS AND THE SLOT RESERVE. The reservation arbitrates between
	// ORDERS — it stops two plans claiming one staging node. It says nothing
	// about a bin physically present with no order behind it: an operator's
	// manual move, a dig parking a blocker. This check is the only one that reads
	// the floor. Neither substitutes for the other, and dropping either leaves a
	// way to arrive at an occupied node.
	//
	// Queue rather than fail, matching the arm above and the house contract: the
	// scanner replays on the next slot-vacancy tick.
	for i, s := range resolvedSteps {
		if s.Action != protocol.ActionDropoff || s.Node == "" || !s.ExclusiveSlot {
			continue
		}
		if finalChecked && s.Node == order.DeliveryNode {
			continue // already asked, immediately above
		}
		// THE ORDER CLEARS THIS NODE ITSELF, EARLIER IN ITS OWN PLAN.
		//
		// A choreography can legitimately place onto a node that is occupied RIGHT
		// NOW, because an earlier leg of the same order carries that bin away
		// first. Keep-staged combined is exactly this: step 1 picks the
		// keep-staged bin off the staging node, step 4 puts the new one back. At
		// dispatch — before any robot has moved — the node is occupied by a bin
		// this very order is about to remove.
		//
		// Gating on that is not caution, it is a self-wedge: the order parks
		// waiting for a node that only the parked order would have cleared, and
		// nothing else ever will. It is the same shape as the 2b05dce deadlock one
		// rung in, with the clearing leg belonging to this order instead of a
		// sibling — and unlike the sibling case, Core CAN see it, because the plan
		// is right here.
		//
		// STRICTLY EARLIER. A later pickup does not help: the bin still has to go
		// down before it can be picked up again, so the node must genuinely be
		// free when we arrive.
		//
		// ASKED OF THE NODE AT STEP i, the way the relay rule and the slot claim
		// ask it (binsAtStep) — and it used to be asked of the plan's steps alone,
		// which cleared a node whose bin some other order holds. Skipped only when
		// this plan takes every bin on the node before it gets there; an empty node,
		// or one still holding a bin this plan will not take, goes to the capacity
		// check as before.
		if remaining, taken, oerr := d.allocator.binsAtStep(order, resolvedSteps, i, s.Node); oerr == nil &&
			len(taken) > 0 && len(remaining) == 0 {
			continue
		}
		if blocked, cap := CheckDropoffCapacity(d.db, s.Node, order.ID); blocked {
			// (a) was asked above. In the branch that would wait, ask (b): the
			// partner that acquired earlier in this pass may be the lift that empties
			// this node before the release lets this leg drop.
			if cap.Cause == CauseDropoffOccupied {
				if st := d.vacatedFor(order, resolvedSteps, i, s.Node, pc); st != nil && st.Partner != 0 &&
					d.noOtherInFlight(s.Node, order) {
					d.admitVacated(order, resolvedSteps, i, st, "exclusive-dropoff gate")
					continue
				}
			}
			// cap.Cause, not the coarse tag — see CauseDropoffCapacity's own note.
			d.setQueueReason(order, protocol.QueueWaitingForSlot, cap.Cause, cap.Params)
			d.dbg("complex: order %d queued — declared-exclusive dropoff %s blocked: %s", order.ID, s.Node, cap.Cause)
			return dispatchStep{done: true, err: fmt.Errorf("dropoff capacity: %s", cap.Cause)}
		}
	}

	// Reserve each concrete storage drop-off SLOT (the destination dual of the bin
	// reserve) — the reservation-native replacement for the retired hard-claim slot
	// loop (the split-brain fix). An incomplete order now holds its slots as
	// revocable RESERVATIONS across ticks, NOT hard nodes.claimed_by. Runs BEFORE the
	// bin reserve (slots-before-bins + the relay rule: a slot must be held before
	// the bin leg reads its emptiness). A fungible NGRP slot conflict
	// reverts-and-re-resolves (the escape valve, preserved); a fixed-concrete
	// conflict holds (Wait) — both write the cause and the scanner parks the order
	// in `queued`.
	//
	// The canonical node-ID sort is gone WITH the loop: the ABBA class dissolves at
	// the soft-acquire layer, where a loser backs off holding only revocable slot
	// reservations, not a hard claim. Removing the loop and its insurance together
	// honors the rule that the slot-ordering must not be reverted without restoring
	// a sweep for slot-wedged orders.
	if slotOutcome, serr := d.allocator.reserveComplexSlots(order, resolvedSteps); serr != nil {
		// The same blank-cause gap as the bin reserve below it, one phase earlier
		// and on an order that has not dispatched. The sibling arm (an incomplete
		// but error-free reserve) has always written CauseComplexSlotReserve; this
		// one wrote nothing, so two branches of one if/else disagreed about whether
		// a wait gets a sentence. A reserve error is a database error: it resolves
		// on its own and the scanner replays.
		if ownerEnded(serr) {
			d.dbg("complex: order %d ended before its slot reserve: nothing held", order.ID)
			return dispatchStep{done: true, err: serr}
		}
		log.Printf("dispatch: complex order %d slot reserve error: %v", order.ID, serr)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseReadFailed,
			QueueParams{Destination: order.DeliveryNode})
		return dispatchStep{done: true, err: serr}
	} else if slotOutcome != reserveComplete {
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseComplexSlotReserve, QueueParams{Destination: order.DeliveryNode})
		d.dbg("complex: order %d held — incomplete slot reserve, retrying next tick", order.ID)
		return dispatchStep{done: true, err: fmt.Errorf("complex order %d slot reserve incomplete", order.ID)}
	}

	return dispatchStep{}
}

// setQueueReason is the dispatch side's door onto the queue-reason columns —
// the package-local name for WriteQueueDetail, which holds the decision.
//
// A METHOD RATHER THAN THE BARE CALL, at ~40 sites, because the store handle,
// the log sink and the subsystem name are the same every time here and are not
// what any of those call sites is about. The body that must not be re-spelled
// lives once, in queue_detail.go, along with the account of what the bool and
// the short-circuit are load-bearing for.
func (d *Dispatcher) setQueueReason(order *orders.Order, code protocol.QueueCode, cause QueueCause, params QueueParams) bool {
	return WriteQueueDetail(d.db, log.Printf, "dispatch", order, code, cause, params, d.NotifyWaitChanged)
}

// NotifyWaitChanged tells the order's station that the reason it is waiting
// changed (WriteQueueDetail decides when). A Core-internal order no station
// placed has nobody to tell.
func (d *Dispatcher) NotifyWaitChanged(order *orders.Order) {
	if order.EdgeUUID == "" || order.StationID == "" || order.ParentOrderID != nil {
		return
	}
	d.emitter.EmitOrderWaitChanged(order.ID, order.EdgeUUID, order.StationID)
}

// SetQueueReason is the exported form, for a door OUTSIDE this package that
// parks an order and then transitions it.
//
// Only the engine's bin-move door needs it, and it needs it for a reason worth
// stating: it wrote the queue detail STRAIGHT TO THE STORE and then called
// Queue(). The transition's history row takes its code from the IN-MEMORY order
// (historyReason), which a direct store write never touches — so the fresh
// `queued` row was born blank, and the only durable record of what a person's
// move was waiting for did not exist. WriteQueueDetail writes both halves; this
// makes that shape reachable from outside rather than growing another spelling
// of it.
func (d *Dispatcher) SetQueueReason(order *orders.Order, code protocol.QueueCode, cause QueueCause, params QueueParams) bool {
	return d.setQueueReason(order, code, cause, params)
}

// failOrderInternal is the scanner-path failure helper. Same as
// failOrder but doesn't take an envelope (no edge-bound reply — the
// edge already has the queued status from intake; it'll learn about
// the failure via EventOrderFailed → edge_handler.HandleOrderError).
func (d *Dispatcher) failOrderInternal(order *orders.Order, code, detail string) {
	if err := d.lifecycle.Fail(order, order.StationID, code, detail); err != nil {
		log.Printf("dispatch: fail order %d: %v", order.ID, err)
		// A refused transition (the order already finished, or another writer
		// moved it first) emits nothing: that order's own transition told the
		// Edge, and "failed" here would contradict a confirm or a cancel. A
		// write error left the order live and unannounced, so the failure is
		// sent explicitly. On the success path fireFailed is the single
		// authoritative emit.
		if !transitionRefused(err) {
			d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, order.StationID, code, detail, order.RecoversOrderID)
		}
	}
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
		// As failOrderInternal: only a write error falls back to an explicit
		// emit; a refused transition emits nothing.
		if !transitionRefused(err) {
			d.emitter.EmitOrderSkipped(order.ID, order.EdgeUUID, order.StationID, code, detail)
		}
	}
}

// transitionRefused reports a lifecycle error that refused the move rather
// than failing to write it: an illegal or concurrent transition, or a wait
// without a cause.
func transitionRefused(err error) bool {
	var illegal IllegalTransition
	var concurrent ConcurrentTransition
	var noCause WaitWithoutCause
	return errors.As(err, &illegal) || errors.As(err, &concurrent) || errors.As(err, &noCause)
}

// SkipOrder is skipOrderInternal for the scanner: a plain move whose named bin
// has left its source ends skipped, the bin it was for having gone.
func (d *Dispatcher) SkipOrder(order *orders.Order, code, detail string) {
	d.skipOrderInternal(order, code, detail)
}

// persistWidenedPlan writes a rewritten step plan back to the order and
// re-extracts its endpoints, which may have shifted when an NGRP step resolved
// to a concrete child. Handler-side lookups (process_node resolution,
// source/delivery rendering) read those columns, so they have to follow the
// steps.
//
// Every write logs and continues rather than failing the dispatch: the plan is
// re-derived from the persisted Group stamp next tick, so a failed write costs a
// re-widen, not the order.
//
// The marshal failure is logged, which it was not before. Discarding the
// widening silently means re-deriving it forever if the plan ever contains
// something unmarshalable, with no cause on the row and no line in the log --
// and every neighbouring write in this block already logs its failure.
func (d *Dispatcher) persistWidenedPlan(order *orders.Order, newSteps []resolvedStep) {
	stepsJSON, mErr := json.Marshal(newSteps)
	if mErr != nil {
		log.Printf("dispatch: marshal widened steps for complex order %d: %v", order.ID, mErr)
	} else if uErr := d.db.UpdateOrderStepsJSON(order.ID, string(stepsJSON)); uErr != nil {
		log.Printf("dispatch: update steps_json for complex order %d: %v", order.ID, uErr)
	} else {
		order.StepsJSON = string(stepsJSON)
	}

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

// revertFungibleDropoffToGroup puts a blocked dropoff step back to the node
// group it was resolved from, so the next tick re-resolves it against a lane
// that can actually take the bin. Reports whether it changed anything.
//
// It is the CAPACITY-block twin of the allocator's reservation-conflict escape
// valve, and it uses the same field for the same reason (resolvedStep.Group).
// The two are deliberately separate call sites rather than one shared helper:
// they fire on different facts — "somebody else reserved the slot" and "there is
// a bin standing on it" — and the allocator's arm has a partial-reserve rollback
// around it that this one must not inherit.
//
// ONLY THE FINAL DROPOFF, and only when it is fungible. A step with no Group was
// named concretely by whoever authored the plan (a loader home, a staging node, a
// line position) and re-picking it would be Core overruling the author — the same
// rule redirectStoreOffDugLane states for an operator's choice. Those keep the
// old behaviour: hold, and retry the one slot.
func (d *Dispatcher) revertFungibleDropoffToGroup(order *orders.Order, steps []resolvedStep) bool {
	idx := -1
	for i, s := range steps {
		if s.Action != protocol.ActionDropoff || s.Node != order.DeliveryNode || s.Group == "" {
			continue
		}
		idx = i // the LAST matching dropoff is the final one, which is what DeliveryNode names
	}
	if idx < 0 {
		return false
	}
	group := steps[idx].Group
	if group == steps[idx].Node {
		return false // already sitting at the group; nothing to revert
	}
	log.Printf("dispatch: complex order %d dropoff %s is occupied — reverting to group %s so the next "+
		"tick picks a slot that can take it", order.ID, steps[idx].Node, group)
	steps[idx].Node = group
	steps[idx].Vacate = nil // housekeeping; plan.StampHonoured would ignore it anyway
	d.persistWidenedPlan(order, steps)
	return true
}

// parkRefusedHand parks a complex order whose hand refused, under the cause the
// refusing leg carries — the same code and cause the split confirm and the lane
// admit wrote for the same refusal — and reports whether err was one. A refused
// hand held nothing and the order is still acquiring: it waits, it does not fail.
// refuseComplexHandover is a complex order's answer to a handover that did not
// send it, and it is the plain path's answer (fulfillment's fresh-bin and
// held-bin arms), clause for clause (C2b Q6):
//
//   - the order ended: nothing was held and nothing is written;
//   - a lost status CAS: another caller owns the order and is dispatching it, so
//     nothing is touched — writing anything here describes the loser's failure
//     on the winner's live row;
//   - the bin it names has gone: skipped, as a plain move is;
//   - a refused hand: nothing was taken; it parks under the refusing leg's cause;
//   - the `dispatched` write failed: nothing was taken; it waits under
//     read-failed;
//   - anything else is a fleet refusal, through the one door
//     (DemoteAfterFleetRefusal) under FleetRefusalCause: `fleet_unavailable`, or
//     `ngrp-resolve` for a synthetic location the fleet was never asked about.
//
// It used to send every error past the first two to failOrderInternal as
// `fleet_failed`, whose engine handler cancels a swap's partner. That ended the
// demand on a robot-system blip (C1 ruling 6) and wrote `failed` over a
// winner's `dispatched` row on a lost CAS. `fleet_failed` is now written for
// nothing.
func (d *Dispatcher) refuseComplexHandover(order *orders.Order, told QueueWait, err error) {
	switch {
	case ownerEnded(err):
		d.dbg("complex: order %d ended before its fleet handover: nothing sent", order.ID)
	case IsConcurrentTransition(err):
		log.Printf("dispatch: complex order %d was dispatched by another caller first — yielding: %v", order.ID, err)
	case errors.Is(err, reservations.ErrNotTheNamedBin):
		d.skipOrderInternal(order, string(protocol.TermNotNeeded),
			fmt.Sprintf("complex order %d: the bin it names is no longer on its source: %v", order.ID, err))
	case d.parkRefusedHand(order, err):
	case isStatusWriteFailed(err):
		log.Printf("dispatch: complex order %d: %v — waiting (soft holds kept)", order.ID, err)
		d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseReadFailed,
			QueueParams{Payload: order.PayloadCode})
	default:
		log.Printf("dispatch: complex order %d was not taken by the fleet, re-queuing (paper kept): %v", order.ID, err)
		code, cause, params := FleetRefusalCause(err, order.DeliveryNode)
		d.DemoteAfterFleetRefusal(order, told, code, cause, params)
	}
}

// isStatusWriteFailed reports a hand whose `→ dispatched` write failed with a
// database error (StatusWriteFailed).
func isStatusWriteFailed(err error) bool {
	var swf *StatusWriteFailed
	return errors.As(err, &swf)
}

func (d *Dispatcher) parkRefusedHand(order *orders.Order, err error) bool {
	r, ok := AsHandRefusal(err)
	if !ok {
		return false
	}
	d.setQueueReason(order, r.Code, r.Cause, r.ParkParams(order))
	d.dbg("complex: order %d held — hand refused at the %s leg (%s): %v", order.ID, r.Leg, r.Cause, r.Err)
	return true
}
