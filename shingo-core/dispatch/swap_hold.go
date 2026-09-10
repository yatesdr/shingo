package dispatch

import (
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// swapHoldVerdict is one swap-gate decision, carrying everything the park needs.
//
// ── WHY THE DECISION AND THE PARK ARE ONE VALUE ───────────────────────────
//
// The arm that made the decision is the only thing that knows what the wait is,
// so it is the only thing that can name it. Both arms of the one remaining gate
// park under `swap-hold` and the caller could in principle hardcode that — but
// hardcoding it at the call site is how a cause and its arm drift apart: the
// next arm added here gets its cause written by a line that never saw the
// decision. The
// verdict carries the cause so that adding an arm cannot silently mis-label a
// park, which is the same "the verdict names the cause" shape the reserve and
// lane-clear doors use.
//
// Re-asking the gate at the park would be a second evaluation of the same
// question against a database that may have moved between them — and a park is
// exactly where the two answers disagreeing is invisible. One decision, one
// value, one write.
type swapHoldVerdict struct {
	held   bool
	reason string     // the engineer-facing log line
	cause  QueueCause // zero when not held
	params QueueParams
}

// swapHold is the verdict for the two arms of the index anti-collision gate,
// which share a cause and a params shape. It served both FACES until Face 1 was
// deleted; the shape is kept because the reason for it has not changed. An arm
// whose wait ends on something other than the sibling would build its own
// verdict rather than call this — the cause is what the releaser row is keyed
// on, and a park under a cause whose releaser sentence does not describe the
// wait is worse than a blank one.
func swapHold(order *orders.Order, reason string) swapHoldVerdict {
	return swapHoldVerdict{
		held:   true,
		reason: reason,
		cause:  CauseSwapHold,
		params: QueueParams{Sibling: order.SiblingOrderUUID},
	}
}

// swapLegHeld is the BOOLEAN VIEW of the gate, for callers that only need the
// answer — which is every test that pins the index anti-collision arm, and
// nothing in production. Kept so those pins keep asking the gate the way they
// always have.
func (d *Dispatcher) swapLegHeld(order *orders.Order, steps []resolvedStep) (bool, string) {
	v := d.swapLegHoldVerdict(order, steps)
	return v.held, v.reason
}

// swapLegHoldVerdict reports whether a coordinated-swap leg must stay queued
// because its commit on the shared LINE node is unsafe until its sibling has
// done its part, and under which cause it parks if so. Returns the zero verdict
// for non-swap orders and any leg whose commit is already safe. Fail-open on
// lookup errors: never freeze a robot on a transient failure.
//
// ── ONE FACE LEFT, AND IT ANSWERS A GEOMETRY QUESTION ─────────────────────
//
// INDEX anti-collision (HOP press-index, 2026-07): a leg that PLACES a bin on
// the line is held until its clearer sibling has DISPATCHED to clear that
// position first — else it drives into a still-occupied node and two bins meet
// on one line position. Scoped to a self-sufficient evac sibling
// (legSecuresOwnReplacement), which is what stops it mutual-holding a pair.
//
// It reads the STEPS, never the mode: what does this leg do to its ProcessNode.
// See swap_leg_role.go.
//
// ── WHY IT SURVIVED THE PAIR RULE AND FACE 1 DID NOT ──────────────────────
//
// The pair rule makes both legs dispatch together, which answers every question
// about whether a partner is COMING. This arm is not that question. It is about
// ORDER — the clearer has to reach the position before the filler puts a bin on
// it — and two legs dispatched in the same pass are still two robots that can
// arrive in either order. Symmetry does not cover sequence.
//
// It earned HOP 07 and it is the owner's to remove, not this batch's. The
// release-layer guard refusePlacingLegWhileSiblingPending covers the same
// collision at the moment the bin actually moves; whether that makes this
// redundant is a question about a hazard with an incident behind it, and the
// honest answer is that it has not been proven, so this stays.
//
// A THIRD FACE WAS BUILT AND BURIED, 2026-08-31, and the note that stood here
// has gone with it — recorded in the U3 commit rather than kept as folklore.
// The short version, because someone will want to build it again: it would have
// held a two_robot supply while its evac sibling was parked pre-dispatch on a
// destination-capacity cause, and its same-resource exemption made it a no-op
// on every claim it could see. Do not rebuild it. The pair rule is the answer
// to the question it was asking, and its exemption is the carve-out the
// no-mode-names ruling forbids.
//
// The directions are asymmetric on purpose. A press-index evac (R1) stages the
// fresh carrier the index leg (R2) later collects, so R1 must be free to run
// first — holding R1 on R2 is the permanent deadlock
// TestSwapHold_PressIndexR1_NotHeldOnItsSibling pins shut. Only the FILLER
// waits; the CLEARER never does.
func (d *Dispatcher) swapLegHoldVerdict(order *orders.Order, steps []resolvedStep) swapHoldVerdict {
	sibUUID, err := d.db.OrderSiblingUUID(order.ID)
	if err != nil {
		// Transient DB read error — fail OPEN. Never freeze a robot on a flaky
		// read; the next scanner tick re-evaluates.
		log.Printf("dispatch: swap-hold sibling lookup for order %d: %v", order.ID, err)
		return swapHoldVerdict{}
	}
	if sibUUID == "" {
		// Not a swap leg. BOTH legs carry the pointer on the wire — Edge mints
		// both uuids before it creates either, "so each leg names its partner and
		// neither goes out unpaired" (protocol.ComplexOrderRequest) — and it is
		// written ATOMICALLY in each leg's CreateOrder INSERT, so an empty pointer
		// reliably means "no sibling".
		//
		// THIS USED TO SAY the pointer rode "the second-created leg's INSERT" and
		// was back-linked onto the first. That was true of an older Edge and is
		// not true now, and the difference is not cosmetic: it made CREATION ORDER
		// a correctness input, which is exactly why the wire was changed. A Core
		// talking to an older Edge still sees the one-way shape, which is why the
		// intake back-link and the on-read repair below both stay.
		//
		// (Which leg is created FIRST is still a per-mode and per-DOOR detail, and
		// still NOT a role: two_robot creates the supply first, a press-index
		// CHANGEOVER creates the supply first, and a STEADY-STATE press-index swap
		// creates R1 — the evac — first. Roles come from the steps; see
		// legTakesLineBin.)
		//
		// We deliberately do NOT fall back to a fail-closed on the step shape
		// alone (gate every leg that pulls the line bin): that shape is shared by
		// the sequential changeover removal (Edge's BuildSequentialRemovalSteps
		// drops at OutboundDestination, not the line) which legitimately has no
		// supply sibling — failing it closed would freeze every sequential removal
		// forever.
		return swapHoldVerdict{}
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

	// ── FACE 1 IS GONE, AND THE ANTI-STRAND IS NOW U1's PAIR RULE ─────────
	//
	// Face 1 held an evac that could not fetch its own replacement until its
	// supply sibling had claimed one, "to prevent stranding" (ALN_003,
	// 2026-06-03). It was guarding a step that moves nothing: dispatch sends a
	// two_robot evac ONE instruction, wait(LINE) — drive to the line and park —
	// because splitAtWait returns steps[:1] and the pickup is appended at
	// release. Nothing this gate could refuse was going to lift a bin.
	//
	// THE ANTI-STRAND MECHANISM IS complex_pair.go. The pair dispatches in one
	// pass or neither leg does, so an evac parked at the line implies its supply
	// was dispatched in the same pass, and "a supply that was never coming" —
	// which is what ALN_003 actually was — is unconstructible. If a comment
	// anywhere still names this arm, or commit 0d95521, as what stops a strand,
	// it is out of date; send the reader here.
	//
	// It also cost a live deadlock on the way out. Testing "supply holds a claim
	// NOW" wedged the ASSY pair on 2026-08-11 (orders 21/22): the supply staged
	// its replacement, the store unclaimed the bin, and the evac waited forever
	// on a claim that had already done its job. That was repaired with
	// swapLegCommittedToFleet rather than removed, which is the arm this deletes.

	// INDEX anti-collision: a leg that drops a bin onto the line waits until its
	// clearer sibling is committed to clearing that position first. Only when the
	// sibling is a self-sufficient evac.
	//
	// THAT NARROWING USED TO BE A DEADLOCK GUARD and is now a scoping one. It
	// read "otherwise this is a two_robot supply whose evac sibling is the one
	// held ABOVE, and holding both deadlocks" — above being Face 1, which is
	// gone, so there is no longer a second hold to mutual-lock with. It stays
	// because it is also the honest scope: a sibling that is NOT a self-
	// sufficient evac is not a clearer, so there is nothing for this filler to
	// wait behind.
	if placesLine && !takesLine {
		if sibErr != nil || sib == nil {
			// Absent/unreadable peer: fail OPEN. The clearer (evac) is created
			// first on this path, so it is normally present; never freeze on a
			// flaky read, and never hold a filler we cannot confirm is paired with
			// a self-sufficient evac.
			return swapHoldVerdict{}
		}
		// A clearer that DIED never cleared the line, so its resident bin is still
		// sitting there and this filler must not drive onto it. Checked before the
		// self-sufficient-evac narrowing below, because that narrowing exists only to
		// avoid mutual-holding two LIVE legs — a dead peer cannot be waiting on us,
		// so there is no deadlock to avoid and every filler shape needs this.
		//
		// IT IS NEARLY UNREACHABLE NOW, AND IT STAYS ANYWAY. It became reachable
		// when the peer-terminal cascade started SPARING a supply parked on a dry
		// source, which let a filler outlive its clearer. The spare is deleted and
		// the death rule is unconditional, so a filler whose clearer died is taken
		// with it — and the window this arm covers is the pass between the clearer
		// going terminal and the death rule reaching this row.
		//
		// A pass is enough to need it. Core cannot recall a driving robot (one-way
		// RDS handoff), so dispatch-time admission is the only lever, and the cost
		// of being wrong here is a bin placed on a position nothing has cleared.
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
				return swapHoldVerdict{}
			}
			// THE SPARED ARM IS GONE WITH THE SPARE. It kept a filler's own
			// material cause on the board when its clearer had died, because the
			// clearer's death used to leave the filler alive and parked. Under the
			// death rule a dead clearer takes this leg with it, so a filler cannot
			// outlive its partner and there is no cause to preserve.
			//
			// This hold now covers only the window between the partner going
			// terminal and the death rule reaching this row — a pass, at most —
			// and it must stay closed for it: the clearer never cleared the line,
			// so its resident is still there and Core cannot recall a driving
			// robot.
			return swapHold(order, "swap: holding filler — clearer sibling died without clearing the line")
		}
		sibSteps, ok := decodeSteps(sib.StepsJSON)
		if !ok || !legSecuresOwnReplacement(sibSteps) {
			// Sibling is not a self-sufficient evac (the two_robot supply case, or
			// an unreadable peer) — nothing here for this filler to wait behind.
			return swapHoldVerdict{}
		}
		if swapLegCommittedToFleet(sib) {
			return swapHoldVerdict{} // evac is committed to clearing the line — release
		}
		// ── AN ACQUIRING CLEARER IS BEING DISPATCHED WITH US, NOT AHEAD OF US ─
		//
		// THIS ARM DEADLOCKED THE PRESS THE MOMENT THE PAIR RULE LANDED, and it
		// was measured, not reasoned about: a steady-state press-index pair (R1
		// the clearer, R2 the filler) sat queued for pass after pass, R1 in
		// `sourcing`, R2 in `queued`, both on swap-hold, no vendor order on
		// either. Both robots and the press out until somebody cancels a leg —
		// which is exactly the permanent mutual hold SYNTH-round2 warned about.
		//
		// The mechanism: this arm holds the filler until its clearer is COMMITTED
		// to the fleet. Under the pair rule, holding the filler parks the WHOLE
		// pair — so the clearer never reaches its own fleet create, never becomes
		// committed, and the condition this waits on can never come true. The
		// gate was written for a world where the two legs dispatched
		// independently, and it is the last thing in the swap gates that still
		// assumed it.
		//
		// A clearer that is still ACQUIRING is one of two things: our partner in
		// a pair pass that is sequencing both of us, or a leg that has not got
		// there yet. In the first case the ordering this arm wants is already
		// guaranteed — dispatchPairInOnePass hands the CLEARER to the fleet
		// before the FILLER, by role read from the steps — and it is guaranteed
		// deterministically rather than by waiting for a state that a wait
		// prevents. In the second the pair rule will not commit either of us
		// anyway.
		//
		// WHAT IS NOT WEAKENED: the dead-clearer arm above still fires, and it is
		// the one that carries the collision hazard that matters — a clearer that
		// never cleared the line leaves its resident sitting there. This arm only
		// ever answered "not yet", and "not yet" is now answered by ordering.
		if protocol.IsAcquiring(sib.Status) {
			return swapHoldVerdict{}
		}
		return swapHold(order, "swap: holding index leg until evac sibling clears the line")
	}

	return swapHoldVerdict{}
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
//
// ONE DIRECTION NOW, NOT TWO. The other was Face 1 — the evac waiting until its
// supply was committed to fetching a replacement, so the line could not strand
// (ALN_003, 2026-06-03) — and it is deleted: the pair rule dispatches both legs
// together, so an evac at the line implies a dispatched supply by construction.
// The paragraph is kept because the SHAPE is the lesson: that direction was
// written to test a LIVE CLAIM and deadlocked the rig the moment the supply
// staged its replacement and the store unclaimed the bin (2026-08-11, orders
// 21/22). Anything added here later must read dispatch STATE, not a claim.
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
