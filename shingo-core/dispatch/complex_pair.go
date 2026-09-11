package dispatch

import (
	"fmt"
	"log"
	"sort"

	"shingo/protocol"
	"shingocore/store/orders"
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
// button, the deferral-and-refire machinery and refusePlacingLegWhileSiblingPending
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
// the supply's own pickup is what frees the slot the evac needs. Under the old
// asymmetry the supply went first and unblocked its partner. Under this rule
// neither goes, so at 100% outbound capacity such a pair waits for something
// else to free a slot. That is the accepted cost: a queued
// pair is a wait, and wait-not-fail is the house law. The named upgrade path is
// a reservation model in which the evac's slot IS the one the supply vacates.
// Do not re-introduce a same-resource exemption to paper over it — that
// exemption is Face 3, and it emptied itself into a no-op precisely because
// every claim it could see was same-resource.

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
// is its partner's ORDER ROW: complex_intake emits EventOrderQueued, the scanner
// runs SYNCHRONOUSLY on that goroutine, and the partner has not been ingested
// yet.
//
// The swap-hold gate handled that asymmetrically and on purpose — fail CLOSED
// for an evac ("hold rather than strand the line"), fail OPEN for a filler — so
// a two_robot supply, which is the leg created first, dispatched alone before
// its evac existed. That gate is deleted; this function is what answers the
// question now. TestSwapPeerTerminalRace_LiveLegResolvesDeadSibling pins that
// the window is real, not theoretical.
//
// Under the pair rule that fail-open is a half-dispatched pair, so it goes. The
// releaser is the partner's own intake: it emits EventOrderQueued, the scanner
// re-runs, the pair is complete, both legs go. Bounded, and no new subscription.
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
}

// dispatchPairInOnePass is the pair rule's body: run every leg's acquisition
// phases, and only when EVERY leg has cleared, hand them all to the fleet.
//
// ── TWO STAGES, AND THE BOUNDARY BETWEEN THEM IS THE RULE ─────────────────
//
// Stage 1 acquires: prepare, gates, destination reserve, source claim, lane
// admit. Stage 2 commits: the fleet create. Nothing physical has been asked of a
// robot until stage 2, so aborting in stage 1 costs a release and a wait, and
// there is no half-committed state to unwind.
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
// dispatches and its partner does not (a stage-2 fleet failure), the partner is
// then the lowest ACQUIRING leg, becomes the leader, and completes itself.
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
		// Not the leader. Say so at debug and touch NOTHING: this is the no-op
		// half of the election, and any write here would be the duplicate pass
		// the election exists to remove.
		d.dbg("complex: order %d defers this pass to pair leader %d", self.ID, legs[0].ID)
		return fmt.Errorf("complex order %d: pair led by order %d this pass", self.ID, legs[0].ID)
	}

	ready := make([]preparedLeg, 0, len(legs))
	for _, leg := range legs {
		steps, st := d.acquireComplexPhases(leg)
		if st.done {
			// ── A PHASE CAN END A LEG, AND THAT IS A DEATH, NOT A PARK ────
			//
			// acquireComplexPhases has terminal exits: a moot reserve skips the
			// leg, a malformed plan fails it. A terminalized leg has already
			// given everything back through TerminalizeOrder, and parking its
			// partner on a cause the dead leg no longer carries (the terminal
			// write clears queue_reason) would leave the partner waiting on
			// something that cannot arrive.
			//
			// So the death rule takes it from here: it decides what a terminal
			// leg means for its partner — cancelled with it, or, for a moot evac,
			// free to go on its own. Run it IN THIS PASS rather than leaving it
			// to the surviving side's next one — applySwapGates would find it
			// eventually, but a pass later, and the partner would spend that
			// pass acquiring for a job that is already over.
			fresh, ferr := d.db.GetOrder(leg.ID)
			if ferr == nil && fresh != nil && protocol.IsTerminal(fresh.Status) {
				// The legs that acquired earlier in this pass hand it back: this
				// pass dispatches nothing, and whether a survivor goes at all is the
				// death rule's call, made below — a survivor that may proceed does
				// so on its own next pass, as a one-leg slice.
				for _, other := range legs {
					if other.ID != leg.ID {
						d.releaseLegHoldings(other, "pair leg died in the pass")
					}
				}
				if kind := swapTerminalKind(fresh.Status); kind != "" {
					d.HandleSwapPeerTerminal(fresh.ID, kind)
				}
				d.dbg("complex: pair leg %d went %s during its phases — the death rule has the pair", leg.ID, fresh.Status)
				return st.err
			}
			if ferr == nil && fresh != nil && fresh.Status == StatusReshuffling {
				for _, other := range legs {
					if other.ID != leg.ID {
						_ = d.parkPairAwaitingPartner(other, fresh)
					}
				}
				d.dbg("complex: pair leg %d pivoted into its own dig — it keeps its lane; its partner waits", leg.ID)
				return st.err
			}
			// THE FIRST REFUSAL PARKS THE PAIR. Every leg gives back what this
			// pass gave it, including the legs that succeeded — a parked pair
			// holds nothing — and both rows are written with the blocked leg's
			// cause.
			d.parkPair(legs, leg)
			return st.err
		}
		ready = append(ready, preparedLeg{order: leg, steps: steps})
	}

	// ── NO ORDERING BETWEEN THE LEGS, ON PURPOSE ──────────────────────────
	//
	// A clearer-before-filler sort stood here. It was the ordering half of the
	// index anti-collision arm — commit the leg that empties the shared position
	// before the leg that fills it, so a robot could not be sent to place onto
	// something nothing had cleared.
	//
	// IT WAS GUARDING A PARKING LOT. Both press-index legs open with a WAIT, so
	// what dispatch sends each robot is "drive to your node and hold". Neither
	// touches a carrier until the operator releases the choreography, and which
	// robot parks first is not a fact about anything: the bins move at RELEASE,
	// in the order the release path chooses. Sequencing the fleet creates was
	// buying an ordering nobody consumes.
	//
	// Same finding as Face 1, and the third time this batch has found it — a
	// dispatch-layer gate written as though dispatch moved material. The
	// collision hazard is real and it lives at release, where
	// refusePlacingLegWhileSiblingPending already orders the legs. That guard
	// needs no help from here.
	for _, p := range ready {
		if err := d.dispatchComplexToFleet(p.order, p.steps); err != nil {
			// The fleet create's failure paths either terminal-fail this leg —
			// and the death rule then resolves its partner (swap_peer.go) — or
			// park it on a read that failed before the create, in which case it
			// is the lowest acquiring leg next pass and completes itself.
			// Nothing to unwind here, and unwinding a leg that may already be
			// moving is the one thing that would be worse than the failure.
			return err
		}
	}
	return nil
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
	for _, leg := range legs {
		if leg.ID == blocked.ID {
			continue
		}
		if leg.QueueReason == src.QueueReason && leg.QueueCode == src.QueueCode && leg.QueueCause == src.QueueCause {
			continue // the short-circuit WriteQueueDetail's own note explains: do not bump updated_at for nothing
		}
		if err := d.db.SetOrderQueueDetail(leg.ID, src.QueueReason,
			protocol.QueueCode(src.QueueCode), src.QueueCause); err != nil {
			log.Printf("dispatch: pair park — mirror cause onto order %d: %v", leg.ID, err)
			continue
		}
		leg.QueueReason, leg.QueueCode, leg.QueueCause = src.QueueReason, src.QueueCode, src.QueueCause
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
	if err := d.db.ReleaseOrderHoldings(leg.ID); err != nil {
		log.Printf("dispatch: %s — release holdings for order %d: %v (reconciliation will sweep it)", why, leg.ID, err)
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
// The releasers are the partner's own transitions: its intake emits
// EventOrderQueued, and a finished dig resumes it through `queued`. A partner
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
