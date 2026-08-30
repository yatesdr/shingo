package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
	"shingocore/store/orders"
)

// isConcreteStorageDropoff reports whether a delivery node is a concrete
// (non-synthetic) STORAGE slot — a direct child of a LANE or NGRP.
// This is the role gate for the complex dropoff-capacity check (#1): such a
// slot must queue-on-full, whereas a LINE/production dropoff must NOT be
// gated (a two-robot supply leg delivers to a line a sibling evac clears, and
// gating it deadlocks). Mirrors engine.isStorageSlot's parent-type rule minus
// the synthetic-root cases — NGRP/LANE dropoffs are handled by step
// re-resolution / ResolutionCapacity before this point.
// (Free function — shared by the Dispatcher's dropoff-capacity gate and the
// Allocator's slotNeeds; it needs only the store handle.)
//
// ── IT DOES NOT COVER STAGING, AND IT USED TO SAY IT DID ──────────────────
//
// This read "STORAGE/STAGING slot" and slotNeeds' docstring said "staging/relay
// included". Neither was true. A staging node is a station with NO PARENT, so
// the `ParentID == nil` guard below rejects it before the LANE/NGRP test is ever
// reached — it was never a question of which parent type, it never got that far.
//
// The cost of the wrong sentence was that nobody re-read the code: the
// predicate's NAME, this comment, and its other caller's comment all promised
// staging coverage, so a reviewer checking "are staging dropoffs gated?" got
// three confirmations and no gate. (An incident attribution stood here; §R.112
// falsified it — see protocol.ComplexOrderStep.ExclusiveSlot. The three
// confirmations and the missing gate are what this paragraph is about, and they
// were real.)
//
// Staging is now covered by DECLARATION instead —
// protocol.ComplexOrderStep.ExclusiveSlot, set by the author of the plan —
// because Core cannot recognise a staging node at all: every station carries the
// one STATION node type, the plantspec Kind is advisory and never persisted, and
// the designation lives in the Edge cell config. This predicate keeps doing the
// job it actually does, under a name that now matches it.
func isConcreteStorageDropoff(db *store.DB, deliveryNode string) bool {
	if deliveryNode == "" {
		return false
	}
	node, err := db.GetNodeByDotName(deliveryNode)
	if err != nil || node == nil || node.IsSynthetic || node.ParentID == nil {
		return false
	}
	parent, err := db.GetNode(*node.ParentID)
	if err != nil || parent == nil {
		return false
	}
	return parent.NodeTypeCode == protocol.NodeClassLANE || parent.NodeTypeCode == protocol.NodeClassNGRP
}

// clearedEarlierInPlan reports whether `prior` — the steps BEFORE the dropoff
// being judged — already picks a bin up from `node`.
//
// It answers one question for the capacity gate: is this node occupied by
// something THIS ORDER is about to carry away? A plan that empties a node and
// then refills it is a choreography, not a conflict, and the occupancy the gate
// would see at dispatch is one the plan has already accounted for.
//
// Callers must pass only the preceding steps. Handing it the whole plan would
// let a LATER pickup excuse an EARLIER dropoff, which is backwards: the bin has
// to go down before anything can pick it up, so a node emptied afterwards was
// still full on arrival.
func clearedEarlierInPlan(prior []resolvedStep, node string) bool {
	if node == "" {
		return false
	}
	for _, p := range prior {
		if p.Action == protocol.ActionPickup && p.Node == node {
			return true
		}
	}
	return false
}

// dispatchStep carries a phase helper's decision back to the DispatchPreparedComplex
// orchestrator. done=true means the phase parked, failed, or skipped the order and
// the orchestrator must return err verbatim; done=false (the zero value) means the
// phase completed and control proceeds to the next phase. It lives only at the
// orchestrator boundary — phases that consume the package's native reserveOutcome
// enum collapse it to a dispatchStep at their return rather than surfacing it up.
type dispatchStep struct {
	done bool
	err  error
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
	// Defense-in-depth: the fulfillment scanner's tryFulfill already gates on
	// IsAcquiring ({queued, sourcing}) before calling here, so a parent in
	// Reshuffling (with a compound in flight), or one already dispatched or
	// terminal, won't reach us through the scanner. Anything calling this
	// directly (engine recovery, future call sites) must still respect the
	// invariant — proceeding on a non-acquiring order would re-dispatch a parent
	// mid-reshuffle or race a post-resume. The acquiring set was widened
	// from queued-only to {queued, sourcing} so a complex order that reached
	// `sourcing` but didn't finish dispatching is retried. Return nil so the caller treats a
	// non-acquiring order as a no-op, not an error.
	if !protocol.IsAcquiring(order.Status) {
		d.dbg("complex: DispatchPreparedComplex called with status=%s (want queued/sourcing); skipping", order.Status)
		return nil
	}

	resolvedSteps, st := d.prepareComplexSteps(order)
	if st.done {
		return st.err
	}

	if st := d.applySwapGates(order, resolvedSteps); st.done {
		return st.err
	}

	if st := d.reserveComplexDestination(order, resolvedSteps); st.done {
		return st.err
	}

	if st := d.acquireComplexSources(order, resolvedSteps); st.done {
		return st.err
	}

	if st := d.admitComplexLanes(order, resolvedSteps); st.done {
		return st.err
	}

	return d.dispatchComplexToFleet(order, resolvedSteps)
}

// admitComplexLanes is the physical question, asked for a coordinated order for
// the first time.
//
// A complex order never went through the scanner's admit — it branches to
// DispatchPreparedComplex on IsCoordinated — and the valve only guards a GATED
// lane. Every lane at both plants is ungated, so the orders that do most of the
// plant's lane work (the changeover swaps) reached the fleet with nothing asked:
// not whether a dig owned the lane, not whether another robot was already inside
// it. The gated arm's version of this hole is recorded beside
// skipsForGatedStoreEntry; this is the same hole on the population that exists.
//
// LAST, after the sources are claimed and the destination reserved, and that
// ordering is deliberate. The refusal is a WAIT, so the order must keep what it
// has while it waits — an order that dropped its claims here would re-race for
// them every pass and could starve behind an order that arrived later.
//
// THE RELEASER is the one every lane wait rides: the order stays acquiring, and
// the scanner re-runs DispatchPreparedComplex on the lane-clearing event set
// (wiring.go) — a placement completing, a bin entering transit, an order
// finishing. Nothing new is subscribed for this.
func (d *Dispatcher) admitComplexLanes(order *orders.Order, resolvedSteps []resolvedStep) dispatchStep {
	v, err := d.admitPlan(order, resolvedSteps, skipsForComplexEntry)
	if err != nil {
		// FAIL CLOSED, with a cause that says so. An unreadable lane is a busy
		// lane; an undetermined answer is Core declining, not a lane that is
		// honestly occupied, and the two are investigated differently.
		log.Printf("dispatch: admission for complex order %d: %v (holding)", order.ID, err)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseAdmissionError,
			QueueParams{Destination: order.DeliveryNode})
		return dispatchStep{done: true, err: err}
	}
	if !v.Admitted() {
		d.dbg("complex: order %d held at lane %s (%s)", order.ID, v.Lane(), v.Cause())
		// A LANE REFUSAL IS NOT A SLOT REFUSAL. Every cause this arm can carry is a
		// fact about a corridor — dig-active, held-source, occupied, target-buried,
		// pickup-elsewhere — and the same causes are filed under
		// QueueStorageRearranging by every other door (complex_reshuffle,
		// planning_service, compound). Filed as QueueWaitingForSlot they rendered
		// "Waiting for a slot" for an order that is waiting for a lane, and one
		// cause read two ways depending on which door parked it.
		//
		// The params move with the code: rearrangingSentence reads Lane and Payload,
		// slotSentence read Destination, so leaving them would have dropped the lane
		// name out of the sentence entirely.
		d.setQueueReason(order, protocol.QueueStorageRearranging, v.Cause(),
			QueueParams{Lane: v.Lane(), Payload: order.PayloadCode})
		// THE REFUSAL ASKS FOR THE CORRIDOR TO BE OPENED, which is what made it
		// safe to ask the reachability question here at all.
		//
		// skipsForComplexEntry used to skip it, and the justification was never
		// "this caller does not need the answer" — it was that a lane-target-buried
		// refusal raised HERE would park the order with nothing to unbury it, for
		// ever, because the complex dig was wired to a finder outcome rather than to
		// an admission verdict. A dig is a service to a LANE now and is proposed
		// WITHOUT consuming the demand, so the refusal can do both: hold the order
		// where it is, and ask for the wall to be taken down.
		//
		// The cause is written ABOVE and is not re-written here (the session-4
		// lesson: the write and the outcome are different moments, and the station
		// is told once per outcome). CauseLaneTargetBuried already names the right
		// releaser — "the bin in front is moved, by a dig or by whoever claimed it
		// carrying it out" — and that sentence is now true by construction.
		if v.Cause() == CauseLaneTargetBuried {
			d.proposeDigForBuriedPickup(order, v.Lane())
		}
		return dispatchStep{done: true, err: fmt.Errorf("complex order %d held at lane %s: %s",
			order.ID, v.Lane(), v.Cause())}
	}

	// ── AND NOW THE MOUTH, WHICH THIS PATH HAD NEVER TAKEN (§R.101) ───────
	//
	// Everything above is admission: ordinary reads, no rows written. The holds are
	// the acquisition, and a coordinated order took none — so §R.95's census found
	// complex acquiring nothing, on the traffic class that carries both plants.
	//
	// Same split, same order, same reasons as AcquireLanesForOrder states for the
	// plain path: the physical questions first because a dig excludes everything,
	// then the mouth, which cannot be lifted out because it runs under the lane's
	// advisory lock inside its own transaction.
	//
	// A conflict is a WAIT, not a fault. The order parks with the contended lane
	// named and the ordinary triggers re-ask; nothing it holds is given up, which
	// is Rule 1 exactly as the plain path applies it.
	holds, hErr := d.resolvePlanLaneHolds(resolvedSteps)
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
		log.Printf("dispatch: resolving lane holds for complex order %d: %v (holding)", order.ID, hErr)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseLaneAcquireError,
			QueueParams{Destination: order.DeliveryNode})
		return dispatchStep{done: true, err: hErr}
	}
	admitted, aErr := d.acquireOrderLanes(order.ID, holds)
	if aErr != nil {
		log.Printf("dispatch: acquiring lanes for complex order %d: %v (holding)", order.ID, aErr)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseLaneAcquireError,
			QueueParams{Destination: order.DeliveryNode})
		return dispatchStep{done: true, err: aErr}
	}
	if !admitted {
		cause := d.causeForLaneHolds(order.ID, holds)
		laneName := d.laneDisplayName(holds)
		d.dbg("complex: order %d could not take the mouth on lane %s (%s)", order.ID, laneName, cause)
		// The mouth refusal is the same family as the admission one above, and its
		// causes (held-dig, held-source, held-traffic, held-unreadable) are filed
		// under QueueStorageRearranging by planning_service. Same reasoning, same
		// params. The two ACQUIRE-ERROR arms above keep QueueWaitingForSlot: those
		// are Core declining to answer, not a lane saying no.
		d.setQueueReason(order, protocol.QueueStorageRearranging, cause,
			QueueParams{Lane: laneName, Payload: order.PayloadCode})
		return dispatchStep{done: true, err: fmt.Errorf("complex order %d could not take lane %s: %s",
			order.ID, laneName, cause)}
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
func (d *Dispatcher) prepareComplexSteps(order *orders.Order) ([]resolvedStep, dispatchStep) {
	var resolvedSteps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &resolvedSteps); err != nil {
		d.failOrderInternal(order, "invalid_steps", fmt.Sprintf("parse stored steps: %v", err))
		return nil, dispatchStep{done: true, err: err}
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
	newSteps, changed, rerr := d.reResolveComplexSteps(resolvedSteps, order.PayloadCode, digAskerFor(order))
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
			return nil, dispatchStep{done: true, err: rerr}
		case ResolutionCapacity:
			capDetail := capacityDetailFrom(payload)
			code := queueCodeForCapacity(capDetail.kindOf())
			d.setQueueReason(order, code, causeForCapacity(capDetail.kindOf(), CauseNGRPResolve),
				queueParamsForCapacity(capDetail, order.PayloadCode, order.DeliveryNode))
			d.dbg("complex: order %d still capacity-blocked at NGRP resolution: %s", order.ID, code)
			return nil, dispatchStep{done: true, err: rerr}
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
	widened, wchanged, hold := d.widenSupplyPickups(order, newSteps)
	newSteps = widened
	changed = changed || wchanged

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
			d.setQueueReason(order, hold.QueueCode, QueueCause(hold.QueueCause), hold.QueueParams)
			d.dbg("complex: order %d supply pickup waiting for material (%s)", order.ID, hold.QueueCause)
			return nil, dispatchStep{done: true, err: fmt.Errorf("supply pickup waiting for material: %s", hold.QueueCause)}
		case OutcomeReshuffle:
			d.dbg("complex: order %d supply pickup buried at widen — bin %d in lane %d",
				order.ID, hold.Buried.Bin.ID, hold.Buried.LaneID)
			d.handleComplexBuriedOnReplay(order, hold.Buried)
			return nil, dispatchStep{done: true, err: fmt.Errorf("supply pickup buried; reshuffle planned")}
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
	// the bin lands — HOME if free, else a buffer slot, else drain — and rewrites
	// DeliveryNode. The Edge shipped DeliveryNode="" and named no target, so Core is
	// the single authority; the release-time redirect overlay (patchRedirectSegments)
	// carries the choice to the fleet. A non-dedicated / non-loader source is left
	// untouched (drains as today). NOT a dispatch gate (no isConcreteStorageDropoff
	// widening) — a resolution-time read, so the swap supply leg is never gated.
	d.placeForDedicatedLoader(order, resolvedSteps)

	return resolvedSteps, dispatchStep{}
}

// acquireComplexSources secures the source bins (Phase D): transition the order
// queued → sourcing, build the plan, reserve the distinct source needs, then
// confirm the complete reserved set to hard claims. done=true means the order was
// moved-concurrently, skipped moot, parked holding partials, held on claim_failed,
// or failed on a malformed order; the orchestrator returns st.err verbatim. The
// MoveToSourcing status side effect is intentional and stays first, inside this
// phase. plan and assigned are locals — nothing downstream needs them. Reads the
// resolved steps read-only.
func (d *Dispatcher) acquireComplexSources(order *orders.Order, resolvedSteps []resolvedStep) dispatchStep {
	// Reserve/confirm. MoveToSourcing at the START of the reserve attempt: the
	// order stays `sourcing` while it holds partials and the scanner retries it
	// (the acquiring-set widening, complex scope). Idempotent — a retried order
	// re-enters sourcing→sourcing every tick, which MoveToSourcing skips. The gates
	// above (swap-hold, capacity, slot-claim) run first and park a blocked order in
	// its entry status (queued first pass, sourcing on retry); both are retried by
	// the complex-scoped scanner, and each wrote queue_reason for the Edge push.
	if err := d.lifecycle.MoveToSourcing(order, "scanner", "reserving source bins"); err != nil {
		// Refused CAS = another actor terminalized or moved this order while we
		// held a stale snapshot. Everything below reserves bins and ends in a
		// fleet dispatch, so yield rather than commit robots for an order that
		// is no longer ours.
		if IsConcurrentTransition(err) {
			log.Printf("dispatch: complex order %d moved under us — another actor owns it now: %v", order.ID, err)
			return dispatchStep{done: true, err: fmt.Errorf("complex order %d moved concurrently: %w", order.ID, err)}
		}
		log.Printf("dispatch: complex order %d → sourcing: %v", order.ID, err)
	}

	// Plan = ordering + intent. RemainingUOP is nil at complex intake (Edge threads
	// it at release, not intake). The plan's predicted bins are advisory; reserve and
	// confirm select/claim against live state, keyed on the plan's distinct needs.
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}
	plan := BuildComplexPlan(resolvedSteps, d.snapshotPickupBins(resolvedSteps), order.PayloadCode, processNode)

	// Reserve = reconcile held reservations against the distinct source needs and
	// soft-hold the gaps (reserveComplexPlan). Runs AFTER the slot-claim loop above,
	// never interleaved with it (slots-before-bins) — one claim class fully ordered
	// before the next is what prevents a slot↔bin cross-type deadlock cycle. Dispatch
	// is gated on a COMPLETE distinct-bin set (the relay rule): an incomplete order
	// holds its partials and stays `sourcing` for the scanner to retry — a robot never
	// starts a job it can't finish, and give-up is operator-driven, never a timer.
	// There is no orphaned-hold window now: the order is already `sourcing` before it
	// holds anything, so a crash leaves a `sourcing` order whose pending holds the
	// owner-liveness reaper reclaims — not a `queued` order stranded with claimed bins.
	assigned, outcome, rerr := d.allocator.reserveComplexPlan(order, plan)
	if rerr != nil {
		// A DATABASE ERROR, AND THE ROW SAYS SO. This arm parked the order in
		// `sourcing` and wrote nothing, so an order stuck behind a database that
		// was not answering looked exactly like one nobody had reached yet — the
		// blank-cause residue the liveness floor kept reporting as "(none)".
		// reserveComplexPlan's errors are all reads and reservation writes; they
		// clear on their own, and the scanner replays.
		log.Printf("dispatch: complex order %d reserve error: %v", order.ID, rerr)
		d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseReadFailed,
			QueueParams{Payload: order.PayloadCode})
		return dispatchStep{done: true, err: rerr}
	}
	switch outcome {
	case reserveMoot:
		// Reserved nothing and every source node is empty — the work is void (e.g. a
		// swap evac whose line bin was removed to quality hold before dispatch). Skip
		// so Edge's HandleOrderSkipped advances the linked changeover task, rather
		// than hold forever: a moot evac is not demand (operator-driven hold-and-retry
		// does not apply).
		d.skipOrderInternal(order, codeNoSourceBin, fmt.Sprintf("complex order %d: no bin at any source node", order.ID))
		return dispatchStep{done: true, err: fmt.Errorf("complex order %d moot — skipped", order.ID)}
	case reserveHolding:
		// Only claim "partial set already held" when the order actually holds part
		// of its set. Holding NOTHING (zero reserved, blocked on every need) is a
		// different operator situation and must not render as a partial hold — that
		// was the SPR ALN_006 lie: "sourcing / partial set already held" while the
		// order held zero reservations and made no progress.
		holdingPartials := len(assigned) > 0
		d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseReserveHolding,
			QueueParams{Payload: order.PayloadCode, Partial: holdingPartials})
		d.dbg("complex: order %d incomplete reserve — holding %d partial(s), retrying next tick", order.ID, len(assigned))
		return dispatchStep{done: true, err: fmt.Errorf("complex order %d reserve incomplete", order.ID)}
	}

	// Confirm = commit the complete reserved set to hard claims (apply-as-confirm, no
	// live re-walk). A claim_failed (a pending hold reaped, or a bin claimed by
	// another order between reserve and confirm) requeues the attempt; a malformed
	// order (no source pickup) fails.
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned); cerr != nil {
		var pe *planningError
		if errors.As(cerr, &pe) && pe.Code == codeClaimFailed {
			d.setQueueReason(order, protocol.QueueWaitingForMaterial, CauseClaimFailed,
				QueueParams{Payload: order.PayloadCode})
			d.dbg("complex: order %d held on claim_failed: %s", order.ID, pe.Detail)
			return dispatchStep{done: true, err: cerr}
		}
		d.failOrderInternal(order, codeNoBin, cerr.Error())
		return dispatchStep{done: true, err: cerr}
	}

	return dispatchStep{}
}

// dispatchComplexToFleet performs the guardless happy-path tail of complex
// dispatch (Phase E): split the resolved steps at the wait boundary, create the
// staged fleet order, transition the order → dispatched, clear any stale
// queue_reason, and emit the dispatched event. All source claims and the
// destination slot reserve are already secured by the earlier phases; this stage
// only turns them into a fleet order. Returns a non-nil error on its two failure
// exits (no actionable blocks before the wait, or the fleet backend rejecting the
// create), each of which terminal-fails the order via failOrderInternal.
func (d *Dispatcher) dispatchComplexToFleet(order *orders.Order, resolvedSteps []resolvedStep) error {
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
	// RUNS ONCE. DispatchPreparedComplex refuses a non-acquiring order, and a
	// successful dispatch leaves the order `dispatched`, so this phase cannot
	// re-enter and re-splice. An earlier phase parking the order means this never
	// ran and nothing was persisted.
	spliced, target, gated, err := d.spliceLaneWait(resolvedSteps)
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
		return err
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
		return rErr
	}
	if gated {
		// One valve, shared with the plain path. nil load sequence: F4c is scoped
		// to the simple transport path and complex has never been expanded.
		if _, gErr := d.dispatchGated(order, target, spliced, order.PayloadCode, nil); gErr != nil {
			d.failOrderInternal(order, "fleet_failed", gErr.Error())
			return gErr
		}
		log.Printf("dispatch: complex order %d dispatched gated into lane %s (%d steps)",
			order.ID, target.lane.Name, len(spliced))
		d.setQueueReason(order, "", "", QueueParams{})
		return nil
	}

	preWait, hasWait := splitAtWait(resolvedSteps)
	vendorOrderID := mintVendorOrderID(order.ID)
	// Complex orders are not load-sequence expanded (nil): the F4c advanced load
	// sequence is scoped to the simple transport path the child-cart delivery
	// uses. Complex is "every other order kind" — byte-identical to before.
	blocks := stepsToBlocks(vendorOrderID, preWait, 0, nil)
	if len(blocks) == 0 {
		d.failOrderInternal(order, "invalid_steps", "no actionable steps before wait")
		return fmt.Errorf("no actionable blocks")
	}

	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   order.Priority,
		RobotGroup: d.robotGroupForPayload(order.PayloadCode),
		Vehicle:    pinnedVehicleFor(order),
		// The claim's routing hints, if it configured any. Nil/empty is SEER
		// auto-pick, which is every order in the plant until one does.
		KeyRoute: order.KeyRoute,
		KeyTask:  order.KeyTask,
		Complete: false, // staged: a multi-wait complex order dwells (Complete=false) until its final segment is released
	}
	d.dbg("complex: creating staged order %s with %d initial blocks (hasWait=%v)", vendorOrderID, len(blocks), hasWait)
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
	if err := d.commitToFleet(order, req, "scanner", d.planNodes(preWait)...); err != nil {
		d.failOrderInternal(order, "fleet_failed", err.Error())
		return err
	}
	if !hasWait {
		// No wait — fleet can complete the order immediately.
		//
		// This now runs AFTER the id write rather than between the create and it.
		// The gap it moves across is two database statements, and nothing reads
		// the vendor's completion flag in between; the create is Complete:false
		// either way, so the fleet cannot finish the order early in that window.
		if err := d.backend.ReleaseOrder(vendorOrderID, nil, true); err != nil {
			log.Printf("dispatch: fleet mark complete failed: %v", err)
		}
	}

	log.Printf("dispatch: complex order %d dispatched as %s (%d steps)", order.ID, vendorOrderID, len(resolvedSteps))
	// Successful dispatch — clear any stale queue_reason from a prior
	// blocked replay attempt.
	d.setQueueReason(order, "", "", QueueParams{})
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, order.SourceNode, order.DeliveryNode)
	return nil
}

// applySwapGates runs the two-robot swap guards (Phase B): the swap peer-terminal
// race unwind (SPR 2424/2425) that resolves a leg whose sibling already went
// terminal, then the swapLegHeld removal-leg hold (ALN_003) that parks an evac leg
// until its supply sibling has secured a claim. done=true means the order was
// resolved by the unwind or parked waiting_for_partner; the orchestrator returns
// st.err verbatim. Reads the resolved steps read-only.
func (d *Dispatcher) applySwapGates(order *orders.Order, resolvedSteps []resolvedStep) dispatchStep {
	// Close the swap peer-terminal RACE (SPR 2424/2425, 2026-07). HandleSwapPeerTerminal
	// unwinds a swap when one leg reaches a terminal state, but it fires from the
	// DEAD leg's side — so if this leg did not exist yet when its sibling died (a
	// supply created + skipped moot in the same tick, before its evac was created),
	// that unwind found no peer and no-op'd, leaving this leg to hold forever on a
	// dead sibling (swapLegHeld waits on a claim that will never come). Re-run the
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
				if self, rerr := d.db.GetOrder(order.ID); rerr == nil && self != nil && protocol.IsTerminal(self.Status) {
					return dispatchStep{done: true, err: fmt.Errorf("complex order %d resolved by swap peer-terminal unwind: sibling %d already %s", order.ID, sib.ID, sib.Status)}
				}
			}
		}
	}

	// Two-robot swap removal-leg hold: don't let a removal (evac) leg that
	// cannot fetch its own replacement claim/pull the line bin until its supply
	// sibling has secured one. Stops a swap from stranding the line when the
	// supermarket is empty (ALN_003 swap-starvation, 2026-06-03). Stay
	// queued — the scanner replays on EventBinUpdated when the supply leg
	// claims, clearing the gate. The sibling pointer is set at intake (the
	// second leg carries it on its ComplexOrderRequest), so it is present
	// here even on the synchronous intake-dispatch path.
	//
	// Reads the RESOLVED steps, not the raw ones: NGRP names have been resolved
	// to concrete nodes by now, and the line node is concrete either way, so the
	// pickup/dropoff shape the gate depends on is stable across resolution.
	// THE CAUSE COMES FROM THE VERDICT, not from this call site. Both faces park
	// under `swap-hold` today, so this site could hardcode it — and that is
	// exactly how a cause and the arm that earned it drift apart: a face added
	// later gets its cause written by a line that never saw the decision. The arm
	// that made the decision is the only thing that can name it — see
	// swapHoldVerdict.
	if v := d.swapLegHoldVerdict(order, resolvedSteps); v.held {
		d.setQueueReason(order, protocol.QueueWaitingForPartner, v.cause, v.params)
		d.dbg("complex: order %d held — %s", order.ID, v.reason)
		return dispatchStep{done: true, err: fmt.Errorf("swap hold: %s", v.reason)}
	}

	return dispatchStep{}
}

// reserveComplexDestination runs the destination-side gates (Phase C): the
// dropoff-capacity gate for concrete STORAGE slots and DECLARED-exclusive
// dropoffs (regression 2b05dce) followed by the reservation-native slot reserve
// (the split-brain fix). done=true means the order parked waiting_for_slot
// (capacity-blocked or incomplete reserve) or hit a reserve error; the
// orchestrator returns st.err verbatim. Runs before the bin reserve
// (slots-before-bins). Reads the resolved steps read-only.
func (d *Dispatcher) reserveComplexDestination(order *orders.Order, resolvedSteps []resolvedStep) dispatchStep {
	// #1 (regression 2b05dce): restore the dropoff-capacity gate for complex
	// orders, but ONLY for concrete STORAGE dropoffs. The scanner dropped the
	// gate for every complex order to unstick two-robot SUPPLY legs — which
	// deliver to a LINE node a sibling EVAC clears — but that also let a
	// changeover drop/evac to a FULL concrete storage slot dispatch into the
	// occupied slot. Gate by node role (storage slot = child of LANE/NGRP), NOT
	// by same-order pickup: gating the line case would re-create the deadlock
	// 2b05dce fixed. NGRP dropoffs are already covered above by
	// reResolveComplexSteps / ResolutionCapacity. Stay queued by returning an
	// error — the scanner keeps the order queued and replays it on the next
	// slot-vacancy tick (same contract as the claim_failed branch below).
	//
	// THIS USED TO SAY "STORAGE/STAGING", AND IT NEVER COVERED STAGING — see
	// isConcreteStorageDropoff's own note. Staging is handled by the declared
	// loop further down instead.
	//
	// AND THE SIBLING PREMISE HAS EXPIRED. The justification above used to end
	// "and Core has no SiblingOrderID to model that". It does now:
	// orders.sibling_order_uuid is stamped at intake (complex_intake.go), linked
	// durably by LinkOrderSiblingsByEdgeUUID, and already read by the swap-hold
	// gate a few lines above. (This named `sibling_order_id`, which is EDGE's
	// column — an INTEGER FK in SQLite. Core's is sibling_order_uuid, TEXT,
	// holding the peer's edge UUID. The two services genuinely differ here and
	// the error travelled as far as the round prompt.) So the reason for making this a blunt role test
	// rather than a modelled dependency no longer holds on its own terms.
	//
	// It is left AS IS deliberately. Re-deriving the line-node rule from the
	// sibling link is a design change with a deadlock on the other side of it,
	// and it is not what the staging fix needed. Recorded here so the next
	// person weighing it starts from what is true rather than re-discovering
	// that the stated obstacle was removed some time ago.
	// finalChecked records whether the DeliveryNode arm below actually ran, so the
	// declared-dropoff loop knows whether that node still needs asking about.
	//
	// IT IS NOT `s.Node == order.DeliveryNode`, and the difference is a hole. An
	// order whose FINAL dropoff is itself a staging node — BuildStageSteps is
	// exactly that shape, pickup source then dropoff staging, nothing after —
	// fails isConcreteStorageDropoff, so this arm skips it; and a loop that then
	// skipped it again for equalling DeliveryNode would leave it checked by
	// nobody. The two arms have to compose on what was ASKED, not on which node
	// it was.
	finalChecked := isConcreteStorageDropoff(d.db, order.DeliveryNode)
	if finalChecked {
		if blocked, cap := CheckDropoffCapacity(d.db, order.DeliveryNode, order.ID); blocked {
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
		if clearedEarlierInPlan(resolvedSteps[:i], s.Node) {
			continue
		}
		if blocked, cap := CheckDropoffCapacity(d.db, s.Node, order.ID); blocked {
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
	// conflict holds (Wait) — both requeue in the order's entry status (queued
	// first pass, sourcing on retry).
	//
	// The canonical node-ID sort is gone WITH the loop: the ABBA class dissolves at
	// the soft-acquire layer, where a loser backs off holding only revocable slot
	// reservations, not a hard claim. Removing the loop and its insurance together
	// honors the rule that the slot-ordering must not be reverted without restoring
	// a sweep for slot-wedged orders.
	if slotOutcome, serr := d.allocator.reserveComplexSlots(order, resolvedSteps); serr != nil {
		// The same blank-cause gap as the bin reserve below it, one phase earlier
		// and on an order that is still `queued`. The sibling arm (an incomplete
		// but error-free reserve) has always written CauseComplexSlotReserve; this
		// one wrote nothing, so two branches of one if/else disagreed about whether
		// a wait gets a sentence. A reserve error is a database error: it resolves
		// on its own and the scanner replays.
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

// setQueueReason is the dispatch side's one door onto the queue-reason columns.
// It generates the operator sentence from code+params (via the shared formatter),
// then writes sentence+code+cause together — so a wait parked here always records
// the structured code, never free text. No-ops when the sentence AND code are
// unchanged: the unchanged short-circuit is load-bearing (rewriting the same
// reason re-touches the row and can re-trigger the very scanner tick that just
// parked the order — an event loop). cause is the engineer-only call-site tag
// (the `where` of older callers); params carries the values the sentence is built
// from and is discarded after formatting. Best-effort: a failed write is logged
// and swallowed (queue_reason is advisory HMI/queue metadata, never a correctness
// gate), leaving the in-memory fields matching the persisted values.
//
// RETURNS WHETHER THIS CALL ACTUALLY WROTE A NEW WAIT, which is a fact only this
// function holds and which one caller needs: the stopped-blocker alarm
// (parkOnClaimedBlocker) fires on the EDGE of a wait rather than on every pass
// that re-asserts it, and the unchanged short-circuit below is exactly that edge.
// A false is either "the row already said this" or "the write failed" — neither
// is a new wait, and both are already logged or harmless. Callers that do not
// care ignore it, which is every other one.
func (d *Dispatcher) setQueueReason(order *orders.Order, code protocol.QueueCode, cause QueueCause, params QueueParams) bool {
	reason := FormatQueueSentence(code, params)
	// The cause is part of what this writes, so it is part of what makes a
	// second call redundant. Comparing only reason and code meant a call that
	// changed nothing but the cause was skipped — which matters where a general
	// reason is set first and a more specific call follows with the same
	// sentence: the buried path sets "storage is being rearranged" on arrival
	// and then narrows the cause to lane-locked or lock-race. Without the cause
	// in this comparison, the narrower tag never lands.
	if order.QueueReason == reason && order.QueueCode == string(code) && order.QueueCause == string(cause) {
		return false
	}
	if err := d.db.SetOrderQueueDetail(order.ID, reason, code, string(cause)); err != nil {
		log.Printf("dispatch: set queue_reason (%s) for order %d: %v", cause, order.ID, err)
		return false
	}
	order.QueueReason = reason
	order.QueueCode = string(code)
	order.QueueCause = string(cause)
	return true
}

// SetQueueReason is the exported form, for a door OUTSIDE this package that
// parks an order and then transitions it.
//
// Only the engine's bin-move door needs it, and it needs it for a reason worth
// stating: it wrote the queue detail STRAIGHT TO THE STORE and then called
// Queue(). The transition's history row takes its code from the IN-MEMORY order
// (historyReason), which a direct store write never touches — so the fresh
// `queued` row was born blank, and the only durable record of what a person's
// move was waiting for did not exist. The three in-package helpers all write
// both halves; this makes that shape reachable from outside rather than growing
// a fourth spelling of it.
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
		// The transition failed, so its fireFailed actionMap hook did NOT emit
		// EmitOrderFailed. Fall back to an explicit emit so the failure still
		// reaches the edge. On the success path fireFailed is the single
		// authoritative emit — emitting again here would double it (the defect
		// this dedup removed).
		d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, order.StationID, code, detail)
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
		// The transition failed, so its fireSkipped actionMap hook did NOT emit
		// EmitOrderSkipped. Fall back to an explicit emit so the skip still
		// reaches the edge. On the success path fireSkipped is the single
		// authoritative emit — emitting again here would double it (the defect
		// this dedup removed, mirroring the failOrderInternal fix above).
		d.emitter.EmitOrderSkipped(order.ID, order.EdgeUUID, order.StationID, code, detail)
	}
}

// proposeDigForBuriedPickup asks for a lane-clear dig on behalf of a complex
// demand whose PICKUP is walled in. Best-effort throughout: every doubt leaves
// the demand exactly as admission left it — parked with a cause and a releaser —
// because the dig is an improvement on the wait, not a precondition for it.
//
// The slot is re-derived through pickupSlotNow rather than carried on the
// verdict. That is the same function admission itself used to reach this refusal,
// so it is one spelling asked twice, not a second opinion; and a verdict that
// carried a slot would be a verdict about a bin rather than about a lane, which
// is not what admission answers.
func (d *Dispatcher) proposeDigForBuriedPickup(order *orders.Order, laneName string) {
	lane, err := d.db.GetNodeByDotName(laneName)
	if err != nil || lane == nil {
		d.dbg("complex: order %d is walled at %s but the lane could not be read (%v) — waiting",
			order.ID, laneName, err)
		return
	}
	target, _, err := d.pickupSlotNow(order, lane)
	if err != nil || target == nil {
		d.dbg("complex: order %d is walled in %s but its pickup slot could not be located (%v) — waiting",
			order.ID, laneName, err)
		return
	}
	// §R.91: this demand is `queued` with no vehicle committed to it, so it takes
	// its own excavation and resumes through `queued` when the corridor opens.
	res := d.proposeLaneClearDig(lane, target, order)
	switch res.outcome {
	case laneClearStarted:
		log.Printf("dispatch: service dig %d created for %s — complex order %d's pickup at %s is "+
			"walled in and admission refused it; the demand keeps waiting with its cause",
			res.parent.ID, lane.Name, order.ID, target.Name)
	case laneClearLaneBusy, laneClearNoShuffleSlot, laneClearBlockerClaimed,
		laneClearNothingInTheWay, laneClearReadFailed, laneClearParkingHeldByDig,
		laneClearEpisodeAlreadyDigging, laneClearLaneOccupied:
		// All of them self-clear and all of them already have a releaser the demand is
		// sitting on. Nothing to arrange and nothing new to tell anyone. Right of
		// way joins the list rather than getting its own arm because this caller
		// reports nothing either way — the demand it is digging on behalf of is
		// already parked with its own cause, which is what this site's header means
		// by "one proposer, two reporting policies". The ranked take's promised
		// refusal (§7) rides this same do-nothing arm for the same reason.
		d.dbg("complex: no dig for %s yet on behalf of order %d (%v)", lane.Name, order.ID, res.err)
	case laneClearNoGroup, laneClearSlotNotInLane, laneClearUnplannable:
		// Geometry. The demand is NOT failed here: admission's refusal is about one
		// lane on one tick, and the demand may still be re-planned onto another
		// source. Loud, because nothing will clear it on its own.
		log.Printf("dispatch: complex order %d is walled in %s and no dig can be planned there (%v) — "+
			"the demand is waiting on a corridor nothing is going to open",
			order.ID, lane.Name, res.err)
	}
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
