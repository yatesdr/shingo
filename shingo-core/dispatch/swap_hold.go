package dispatch

import (
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// swapLegHeld reports whether a coordinated-swap leg must stay queued because its
// commit on the shared LINE node is unsafe until its sibling has done its part.
// One gate, two faces of the same invariant — never let one leg physically commit
// on the shared node while its partner cannot satisfy the precondition that makes
// that commit safe. Returns (false, "") for non-swap orders and any leg whose
// commit is already safe. Fail-open on lookup errors: never freeze a robot on a
// transient failure.
//
// The two faces, both keyed on what the leg does to its ProcessNode (the shared
// line position), read from the steps — see swap_leg_role.go:
//
//   - EVAC anti-strand (ALN_003, 2026-06-03): a leg that TAKES the line bin and
//     does NOT secure its own replacement is held until its supply sibling has
//     CLAIMED a replacement — else it pulls the line's bin with nothing coming and
//     strands the line empty.
//
//   - INDEX anti-collision (HOP press-index, 2026-07): a leg that PLACES a bin on
//     the line is held until its clearer sibling has DISPATCHED to clear that
//     position first — else it drives into a still-occupied node (two bins on one
//     line position). Scoped to a self-sufficient evac sibling
//     (legSecuresOwnReplacement) so it can never mutual-hold the evac case: a
//     two_robot supply's sibling is a plain evac already held on the supply, and
//     holding both would deadlock.
//
// The directions are asymmetric on purpose. A press-index evac (R1) stages the
// fresh carrier the index leg (R2) later collects, so R1 must be free to run
// first — holding R1 on R2 is the permanent deadlock
// TestSwapHold_PressIndexR1_NotHeldOnItsSibling pins shut. Only the FILLER waits;
// the CLEARER never does.
//
// Role is read from the steps, not geometry. It used to be
// `DeliveryNode != ProcessNode`, a different question: Core derives DeliveryNode
// from the steps (extractEndpoints = last pickup-or-dropoff), so a press-index R1
// — ending by staging a carrier at the index node — looked like a removal leg
// needing help. It isn't; it fetches that carrier itself.
func (d *Dispatcher) swapLegHeld(order *orders.Order, steps []resolvedStep) (bool, string) {
	sibUUID, err := d.db.OrderSiblingUUID(order.ID)
	if err != nil {
		// Transient DB read error — fail OPEN. Never freeze a robot on a flaky
		// read; the next scanner tick re-evaluates.
		log.Printf("dispatch: swap-hold sibling lookup for order %d: %v", order.ID, err)
		return false, ""
	}
	if sibUUID == "" {
		// Not a swap leg. The sibling pointer is written ATOMICALLY in the
		// second-created leg's CreateOrder INSERT (domain.Order.SiblingOrderUUID)
		// and back-linked onto the first at that leg's intake, so a two-robot leg
		// can no longer reach here with an empty pointer because a post-create
		// link step failed — an empty pointer now reliably means "no sibling".
		//
		// (Which leg is created second is a per-mode detail and NOT a role:
		// two_robot creates the supply first, press-index creates the evac first.
		// Roles come from the steps — see legTakesLineBin.)
		//
		// We deliberately do NOT fall back to a fail-closed on the step shape
		// alone (gate every leg that pulls the line bin): that shape is shared by
		// the sequential changeover removal (Edge's BuildSequentialRemovalSteps
		// drops at OutboundDestination, not the line) which legitimately has no
		// supply sibling — failing it closed would freeze every sequential removal
		// forever.
		return false, ""
	}
	sib, sibErr := d.db.GetOrderByUUID(sibUUID)

	// On-read repair of the bidirectional link: if the peer's back-link is
	// missing — e.g. the intake back-link write failed, or this row arrived
	// before its peer — heal it now that both rows exist, so the peer-death
	// handler can find either leg from the other. Idempotent and gated on
	// "actually missing" so we don't re-touch the rows on the happy path.
	//
	// This runs BEFORE the shape test, for EVERY swap leg, deliberately: healing
	// the link is not the hold gate's business. It used to sit below the gate, so
	// it only ever ran for legs the gate considered removals — which now excludes
	// press-index entirely (R1 is self-sufficient, R2 is a supply), and would have
	// silently dropped the repair for that whole mode.
	if sibErr == nil && sib != nil && sib.SiblingOrderUUID != order.EdgeUUID {
		if _, rerr := d.db.LinkOrderSiblingsByEdgeUUID(order.EdgeUUID, sibUUID); rerr != nil {
			log.Printf("dispatch: swap back-link repair for order %d sib %s: %v", order.ID, sibUUID, rerr)
		}
	}

	takesLine := legTakesLineBin(steps, order.ProcessNode)
	placesLine := legPlacesLineBin(steps, order.ProcessNode)

	// EVAC anti-strand: a leg that lifts the line's bin but cannot fetch its own
	// replacement waits for its supply sibling to claim, or the line strands.
	if takesLine && !legSecuresOwnReplacement(steps) {
		if sibErr != nil || sib == nil {
			// Supply row should exist (created first, linked at intake); hold
			// rather than strand the line if it is somehow missing.
			return true, "swap: awaiting supply sibling"
		}
		claimed, err := d.db.ListBinsByClaim(sib.ID)
		if err != nil {
			log.Printf("dispatch: swap-hold claim check for order %d sib %d: %v", order.ID, sib.ID, err)
			return false, ""
		}
		if len(claimed) > 0 {
			return false, "" // supply is holding a replacement right now — release
		}
		// ── AND A SUPPLY THAT HAS ALREADY DELIVERED ITS REPLACEMENT COUNTS ──
		//
		// "Secured" was tested as "holds a claim NOW", and those agree only until
		// the supply STAGES its replacement: the store unclaims the bin, the claim
		// disappears, and this arm held the evac forever waiting for a claim that
		// had already done its job. Delivered is a STRONGER form of secured than
		// in-hand, and it read as weaker.
		//
		// The deadlock, live on the rig 2026-08-11: ASSY two_robot, orders 21 and
		// 22. The supply picked an empty at LSC_020, dropped it at staging SLN_002,
		// released its claim, and drove on toward the line — where it could not
		// place, because the line still held the old bin. The evac that would have
		// removed that bin was held here, on "supply sibling claims a bin", against
		// a supply holding zero claims. Each waited for the other; AMR-05 stood
		// still; WELD-1 stopped producing ASSY; the panel loop stopped consuming
		// and drained SYN_STAMP's carriers to zero. One predicate, eight wedged
		// orders.
		//
		// swapLegCommittedToFleet solved exactly this for the MIRROR direction and
		// says so in its own doc — "read from dispatch state, NOT a live claim, so
		// the hold releases correctly even after the clearer completes and drops
		// its claim". This arm never got the same treatment. It does now, through
		// the same predicate, so the two directions cannot drift.
		//
		// Committing to the fleet is a safe threshold rather than an optimistic
		// one: a complex order claims its source bins BEFORE the fleet create
		// (acquireComplexSources → confirmComplexPlan → dispatchComplexToFleet), so
		// `dispatched` already implies a replacement was claimed. The acquiring
		// states it is held FROM read as not-committed, which is the case the guard
		// was written for (ALN_003, 2026-06-03: never pull the line bin while the
		// supermarket might be empty).
		if swapLegCommittedToFleet(sib) {
			return false, "" // supply already secured and staged/delivered it — release
		}
		return true, "swap: holding removal leg until supply sibling secures a bin"
	}

	// INDEX anti-collision: a leg that drops a bin onto the line waits until its
	// clearer sibling is committed to clearing that position first. Only when the
	// sibling is a self-sufficient evac — otherwise this is a two_robot supply
	// whose evac sibling is the one held (above), and holding both deadlocks.
	if placesLine && !takesLine {
		if sibErr != nil || sib == nil {
			// Absent/unreadable peer: fail OPEN. The clearer (evac) is created
			// first on this path, so it is normally present; never freeze on a
			// flaky read, and never hold a filler we cannot confirm is paired with
			// a self-sufficient evac.
			return false, ""
		}
		// A clearer that DIED never cleared the line, so its resident bin is still
		// sitting there and this filler must not drive onto it. Checked before the
		// self-sufficient-evac narrowing below, because that narrowing exists only to
		// avoid mutual-holding two LIVE legs — a dead peer cannot be waiting on us,
		// so there is no deadlock to avoid and every filler shape needs this.
		//
		// This became reachable when the peer-terminal cascade stopped cancelling a
		// supply parked on a dry source (swap_peer.go, "spare a swap supply parked on
		// a dry source"): that cancel was what previously kept a filler from
		// outliving its clearer. Sparing the wait is right — it stops the re-arm
		// churn — but the filler must then be held rather than left free to dispatch
		// once the operator stocks the payload. Core cannot recall a driving robot
		// (one-way RDS handoff), so dispatch-time admission is the only lever.
		//
		// Terminal-SUCCESS is not this case: a confirmed evac did clear the line and
		// is released by swapLegCommittedToFleet below. swapTerminalKind is non-empty
		// only for skipped/failed/cancelled.
		if kind := swapTerminalKind(sib.Status); kind != "" {
			// A SKIPPED clearer is MOOT, not dead, and the difference is the whole
			// point of this hold. The guard exists because a clearer that died never
			// cleared the line, so its resident bin is still sitting there and this
			// filler must not drive onto it. A clearer is skipped for the opposite
			// reason: it found NO bin to clear. The line is empty, there is nothing
			// to collide with, and this filler is the thing that should put a carrier
			// back on it.
			//
			// HandleSwapPeerTerminal has always said exactly this — "a moot (skipped)
			// evac is a clean no-op — the line's resident was already gone, so the
			// supply proceeds" — and this arm contradicted it. The peer handler let
			// the filler go and the hold caught it again on the next scan, so the
			// filler sat queued with a reason describing a death that had not
			// happened. Observed the moment the moot narrowing started skipping these
			// evacs at all: order 64 skipped, order 65 held on it indefinitely.
			if kind == SwapTerminalSkipped {
				return false, ""
			}
			return true, "swap: holding filler — clearer sibling died without clearing the line"
		}
		sibSteps, ok := decodeSteps(sib.StepsJSON)
		if !ok || !legSecuresOwnReplacement(sibSteps) {
			// Sibling is not a self-sufficient evac (the two_robot supply case, or
			// an unreadable peer) — its evac sibling is held on us; do not mutual-hold.
			return false, ""
		}
		if swapLegCommittedToFleet(sib) {
			return false, "" // evac is committed to clearing the line — release
		}
		return true, "swap: holding index leg until evac sibling clears the line"
	}

	return false, ""
}

// swapLegCommittedToFleet reports whether a swap sibling has committed to the
// fleet — it holds a vendor order and is en route or done.
//
// ── ONE PREDICATE, BOTH DIRECTIONS, AND THAT IS THE POINT ─────────────────
//
// Read from dispatch state, NOT a live claim, so a hold releases correctly even
// after the sibling completes its part and drops its claim. Both swap holds need
// exactly that:
//
//	CLEARER  the filler waits until the evac is committed to clearing the shared
//	         line position, so the fleet can sequence the drop after the pickup.
//	SUPPLY   the evac waits until the supply is committed to fetching a
//	         replacement, so the line cannot strand (ALN_003, 2026-06-03).
//
// The supply side used to test a LIVE CLAIM instead, and deadlocked the moment
// the supply staged its replacement and the store unclaimed the bin — see the
// call site. Sharing the predicate is what stops the two directions drifting
// again.
//
// The acquiring states (queued/sourcing) a leg is held FROM read as
// not-committed, and so do the failure states where it will not do its part; a
// faulted sibling may recover, so the held leg stays held.
//
// ── RESHUFFLING IS AN EXPLICIT ARM, AND IT WAS CORRECT BY ACCIDENT ────────
//
// A leg waiting on a dig wears `reshuffling`. It landed in `default` and read as
// NOT COMMITTED, which is the right answer — it fails both halves of what this
// predicate means: it holds no vendor order, and it is not en route. But the
// paragraph above enumerates the not-committed cases as "acquiring" and
// "failure", and `reshuffling` is neither. Nothing recorded the decision because
// nobody had made one.
//
// That is a live hazard rather than a tidiness point. The plausible mistake is
// specific and someone will make it: a leg in `reshuffling` is visibly DOING
// something — robots are moving, blockers are being carried out — so adding it
// to the committed list reads as a correction. It is not. Committed means
// committed TO THIS SWAP'S OWN WORK, and a leg mid-dig has not started that
// work; releasing its partner early is the line clearing with no replacement
// coming, which is ALN_003 (2026-06-03), the incident the supply-side hold
// exists for.
//
// IT IS ABOUT TO MATTER MUCH MORE. Under §R.91 the demand that raises a dig
// BECOMES the dig's parent and wears `reshuffling` while it runs — so a swap leg
// in `reshuffling` goes from a state this predicate never really saw to an
// ordinary one. Round 2 flagged it for exactly that reason: settle it before the
// unification arrives and finds it undecided.
//
// Written as its own arm rather than folded into `default` so that a future
// editor has to delete a paragraph to get the answer wrong, instead of adding a
// constant to a list.
//
// ── THERE IS A SECOND READER, AND IT READS `false` THE OPPOSITE WAY ───────
//
// handOffDugLane's gate 3 asks this same question of a LANE HOLDER: has the
// demand already collected, so that the corridor can be given back. Everything
// above is written in swap vocabulary, and the two callers agree on what "not
// committed" MEANS while disagreeing completely on what it COSTS:
//
//	HERE          not committed → the partner keeps waiting. Erring this way
//	              costs a wait, so `default` is the safe arm.
//	GATE 3        not committed → convert the dig row and keep the corridor.
//	              Erring this way is the leaked hold that wedged the plant.
//
// So before adding a status to `default` — or moving one out of the committed
// list — check gate 3 as well: a status that is merely "not yet doing its part"
// to a swap sibling may be "has already been to the lane and left" to a corridor.
// `faulted` is exactly that status, and gate 3 handles it LOCALLY rather than
// here, because this caller's answer for it is the right one.
func swapLegCommittedToFleet(sib *orders.Order) bool {
	switch sib.Status {
	case StatusDispatched, StatusInTransit, StatusStaged, StatusDelivered, StatusConfirmed:
		return true
	case StatusReshuffling:
		// Mid-dig. No vendor order, not en route: the partner keeps waiting.
		return false
	default:
		return false
	}
}

// swapTerminalKind maps a terminal order status to the SwapTerminal* kind
// HandleSwapPeerTerminal expects, or "" when the status is not a swap-relevant
// terminal — the surviving-side race check skips the unwind for a non-terminal
// or unmapped sibling.
func swapTerminalKind(status protocol.Status) string {
	switch status {
	case StatusSkipped:
		return SwapTerminalSkipped
	case StatusFailed:
		return SwapTerminalFailed
	case StatusCancelled:
		return SwapTerminalCancelled
	default:
		return ""
	}
}
