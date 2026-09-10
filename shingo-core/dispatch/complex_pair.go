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
// unconstructible — if the evac is parked at the line, the supply was dispatched
// in the same pass.
//
// So RELEASE IS UNCHANGED and must stay so. ComputeSwapReady, the RELEASE
// button, the deferral-and-refire machinery and refusePlacingLegWhileSiblingPending
// are not this file's business and are not touched by it.
//
// ── NO MODE NAMES. NOT ONE ────────────────────────────────────────────────
//
// The rule reads the PAIR STRUCTURE — that the order names a sibling — and never
// `SwapMode`. That is deliberate and it is the second ruling: two_robot,
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
// else to free a slot. That is the accepted cost, ruled on knowingly: a queued
// pair is a wait, and wait-not-fail is the house law. The named upgrade path is
// a reservation model in which the evac's slot IS the one the supply vacates.
// Do not re-introduce a same-resource exemption to paper over it — that
// exemption is Face 3, and it emptied itself into a no-op precisely because
// every claim it could see was same-resource.

// coordinatedPairLegs returns the acquiring legs of order's coordinated pair, in
// ascending order-id order, plus whether a declared partner's ROW is not there
// yet.
//
// Three answers, and the caller must distinguish all three:
//
//	(nil,  false)  a solo order. Runs the ordinary per-order phases.
//	(nil,  true)   this order names a partner and the partner's row does not
//	               exist yet. The pair is incomplete; nothing dispatches.
//	(legs, false)  the pair, filtered to the legs still acquiring.
//
// ── THE POINTER IS PRESENT BEFORE THE ROW IS, AND THAT IS THE WHOLE POINT ─
//
// protocol.ComplexOrderRequest.SiblingOrderUUID rides BOTH legs — Edge mints
// both uuids before it creates either — so the pointer is never the thing that
// is missing. What IS missing, for the leg created first, is its partner's ORDER
// ROW: complex_intake emits EventOrderQueued, the scanner runs SYNCHRONOUSLY on
// that goroutine, and the partner has not been ingested yet.
//
// swapLegHoldVerdict handled that asymmetrically and on purpose — fail CLOSED
// for an evac ("hold rather than strand the line"), fail OPEN for a filler — so
// a two_robot supply, which is the leg created first, dispatched alone before
// its evac existed. TestSwapPeerTerminalRace_LiveLegResolvesDeadSibling pins
// that the window is real, not theoretical.
//
// Under the pair rule that fail-open is a half-dispatched pair, so it goes. The
// releaser is the partner's own intake: it emits EventOrderQueued, the scanner
// re-runs, the pair is complete, both legs go. Bounded, and no new subscription.
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
func (d *Dispatcher) coordinatedPairLegs(order *orders.Order) ([]*orders.Order, bool) {
	sibUUID, err := d.db.OrderSiblingUUID(order.ID)
	if err != nil {
		// Transient read error — fail OPEN to the solo path, the same way the
		// swap gate has always failed open on this read. Never freeze a robot on
		// a flaky read; the next scanner tick re-asks.
		log.Printf("dispatch: pair lookup for order %d: %v (treating as solo this pass)", order.ID, err)
		return nil, false
	}
	if sibUUID == "" {
		return nil, false // not a pair
	}
	sib, sibErr := d.db.GetOrderByUUID(sibUUID)
	if sibErr != nil {
		log.Printf("dispatch: pair partner read for order %d (%s): %v", order.ID, sibUUID, sibErr)
		return nil, true // unreadable partner is an incomplete pair, not a solo order
	}
	if sib == nil {
		return nil, true // declared partner, row not ingested yet
	}
	legs := make([]*orders.Order, 0, 2)
	for _, leg := range []*orders.Order{order, sib} {
		if protocol.IsAcquiring(leg.Status) {
			legs = append(legs, leg)
		}
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].ID < legs[j].ID })
	return legs, false
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
// It means the partner has left the acquiring set — committed to the fleet
// already, or terminal. Committed is the completion path and the leg should go.
// Terminal is the death rule's business, and applySwapGates already runs the
// peer-terminal unwind from the surviving side, so the leg is resolved inside
// its own phases rather than by a test here.
func (d *Dispatcher) dispatchPairInOnePass(self *orders.Order, legs []*orders.Order) error {
	if legs[0].ID != self.ID {
		// Not the leader. Say so at debug and touch NOTHING: this is the no-op
		// half of the election, and any write here would be the duplicate pass
		// the election exists to remove.
		d.dbg("complex: order %d defers this pass to pair leader %d", self.ID, legs[0].ID)
		return fmt.Errorf("complex order %d: pair led by order %d this pass", self.ID, legs[0].ID)
	}

	type preparedLeg struct {
		order *orders.Order
		steps []resolvedStep
	}
	ready := make([]preparedLeg, 0, len(legs))
	for _, leg := range legs {
		steps, st := d.acquireComplexPhases(leg)
		if st.done {
			// THE FIRST REFUSAL PARKS THE PAIR. Every leg gives back what this
			// pass gave it, including the legs that succeeded — a parked pair
			// holds nothing — and both rows are written with the blocked leg's
			// cause.
			d.parkPair(legs, leg)
			return st.err
		}
		ready = append(ready, preparedLeg{order: leg, steps: steps})
	}
	for _, p := range ready {
		if err := d.dispatchComplexToFleet(p.order, p.steps); err != nil {
			// The fleet create's own failure path already terminal-fails this
			// leg, and a terminal leg takes its siblings with it (swap_peer.go).
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
// thing that can name it (see swapHoldVerdict's own note), and re-deriving here
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
		if err := d.db.ReleaseOrderHoldings(leg.ID); err != nil {
			log.Printf("dispatch: pair park — release holdings for order %d: %v "+
				"(reconciliation will sweep it)", leg.ID, err)
		}
		if err := d.ReleaseLanesForOrder(leg.ID); err != nil {
			log.Printf("dispatch: pair park — release lanes for order %d: %v", leg.ID, err)
		}
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

// parkPairAwaitingPartner parks a leg whose declared partner has no row yet.
//
// The pair cannot be evaluated, so nothing dispatches. It holds nothing for the
// same reason the rest of the rule holds nothing, and it parks under
// CauseSwapHold — which is now that cause's ONLY producer: every other swap wait
// names the physical thing the pair is short of, and this one genuinely is
// waiting on the sibling itself.
//
// The releaser is the partner's own intake, which emits EventOrderQueued.
func (d *Dispatcher) parkPairAwaitingPartner(order *orders.Order) error {
	if err := d.db.ReleaseOrderHoldings(order.ID); err != nil {
		log.Printf("dispatch: awaiting-partner park — release holdings for order %d: %v", order.ID, err)
	}
	if err := d.ReleaseLanesForOrder(order.ID); err != nil {
		log.Printf("dispatch: awaiting-partner park — release lanes for order %d: %v", order.ID, err)
	}
	d.setQueueReason(order, protocol.QueueWaitingForPartner, CauseSwapHold,
		QueueParams{Sibling: order.SiblingOrderUUID})
	d.dbg("complex: order %d holding — coordinated partner %s has no order row yet", order.ID, order.SiblingOrderUUID)
	return fmt.Errorf("complex order %d: coordinated partner %s not ingested yet", order.ID, order.SiblingOrderUUID)
}
