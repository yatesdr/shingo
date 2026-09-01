package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// Admission — one decision, asked in one place.
//
// "May this move happen now" is currently asked at eleven sites in four
// spellings, and the fold multiplies that by the number of blockers in a lane:
// today a reshuffle asks once and commits N moves, after the fold it asks N
// times. Any disagreement between the spellings gets multiplied with it.
//
// ── WHAT THIS ANSWERS, AND WHAT IT DOES NOT ───────────────────────────────
//
// IT ANSWERS LANE SAFETY: is the lane claimable by this order, is a robot
// already inside it, is the slot this order wants actually reachable.
//
// IT IS NOT THE ORDERING QUESTION. "May this move happen" and "which of the
// waiting ones goes first" are different, and the tiered depth-first classifier
// (classifyLaneEntry, lane_entry.go) answers the second. The line between them:
// ADMISSION says the lane cannot take this move; ORDERING says it could, but
// somebody else should go first. lane-target-buried is admission — the bin
// physically cannot be reached. lane-deeper-pending is ordering — the move is
// perfectly safe, and doing it now would wall a deeper target. The two look
// alike in a queue_cause histogram and are not alike at all, which is why the
// constants are grouped by that split in queue_cause.go.
//
// IT IS NOT THE MOUTH ACQUIRE. admitMouth (store/reservations) stays where it
// is: its correctness is a property of the advisory lock it is called under and
// of running inside the acquire transaction, not of the function. Lifting it
// would either lose that or hold a database transaction open across a decision.
// It is also the only WRITE in the family, and a read-only decision must not
// grow a side effect. Admission is asked BEFORE the acquire; the acquire still
// arbitrates.
//
// IT IS NOT SOURCE RESOLUTION. "Does a source bin exist, does a destination
// slot exist" belong to the finder and the placer, which answer them with their
// own vocabulary of finder-* causes and their own retry semantics.
//
// ── WHO ASKS WHAT: the eleven sites, and where each landed ────────────────
//
// The convergence's whole risk is a site that LOOKS like this question and is
// not. Written down here because a boundary that lives only in a brief stops
// being consulted the moment the brief is closed.
//
// DELEGATES (asks exactly this question) — EVERY lane-ENTRY path, which is the
// convergence. Each declares its own skip set (admissionSkips); none carries its
// own copy of a physical question any more:
//   - AdvanceCompoundOrder — a reshuffle leg against its source and dest lanes.
//     Skips nothing.
//   - AcquireLanesForOrder — a plain order, before the mouth acquire. Reached by
//     the scanner's two callers, by the core-operator BIN MOVE door
//     (engine.CreateBinMove), and by REDIRECT, which points a live order at a
//     node its original admission said nothing about. skipsForEntry decides the
//     set from the caller's kind; the reachability skip carries a partial
//     justification and is the open item — see the audit on skipsForPlainEntry.
//   - laneGateRetrieveCause — a gate-staged retrieve, at dispatch and on every
//     evaluator pass. Skips nothing, since the unification.
//   - gateEntryVerdict — a gate-staged store, at the valve and every evaluator
//     pass. skipsForGatedStoreEntry, which is empty.
//   - admitComplexLanes — a coordinated order's whole plan, before its fleet
//     create (admitPlan walks the steps). skipsForComplexEntry. This was the
//     largest hole: a complex order goes nowhere near the scanner's admit, and
//     the valve only stands in front of a GATED lane, so on the ungated lanes
//     both plants actually run there was nothing between a changeover swap and a
//     corridor.
//
// CANNOT REACH A LANE AT ALL, which is the other way to satisfy the rule:
//   - the manual robot move (www/handlers_robots.go apiRobotMoveTo) refuses a
//     lane destination STATICALLY — geometry, not state, never rather than
//     not-yet. It creates no order, so it has nothing to park, nothing to
//     release and nothing to wake it; an ask could only refuse-and-forget, and a
//     PASS would put an unrecorded robot in a corridor the next order's
//     occupancy read cannot see. The genuine ungoverned move for maintenance is
//     the vendor's own console, outside Core.
//
// THE EXCEPTION LIST IS EMPTY. Every path that can put a robot in a lane either
// asks admission or cannot reach a lane by construction. A path that is neither
// is a finding, not a fourth category.
//
// ASKS A DIFFERENT QUESTION, and must not delegate:
//   - the three reshuffle planners (planning_service.go, complex_reshuffle.go
//     ×2) ask "may I CLAIM this lane for a dig". Delegating would refuse every
//     plan: a buried retrieve is buried by construction, so the reachability arm
//     rejects the very thing the reshuffle exists to fix.
//   - laneEntryCause / classifyLaneEntry ask ORDERING — whose turn, not whether.
//   - the finder's post-find buried check asks find-time reachability, with the
//     finder's own retry semantics and finder-* causes.
//   - ListChildNodesUnlocked filters dig-held lanes out of a PLAN-TIME candidate
//     query. Same fact, different moment, and it rides a query already running.
//   - LaneAcceptsInbound is an advisory RANKING hint, taken without the lane's
//     lock and documented as such; the mouth gate still arbitrates.
//
// IS NOT A DECISION AT ALL:
//   - admitMouth is the acquisition, not a decision about it (see below).
//   - causeForLaneHolds LABELS a refusal already made.
//   - IsSlotAccessible / BlockersInFrontOf are primitives this function calls.
//
// ── AUDIT: where a PLAIN order gets each of these questions answered ──────
//
// AcquireLanesForOrder now delegates the PHYSICAL questions and still does not
// delegate the MOUTH, and that distinction is the whole of the boundary. The
// mouth verdict is on admission's INSIDE — admitMouth runs under
// pg_advisory_xact_lock inside the acquire transaction (mouth.go:62-109,120) —
// so lifting it would either lose the lock or hold a transaction open across a
// decision. The physical questions have no such tie: they are ordinary reads,
// and asking them through admission is what the convergence did.
//
// So the site is both a caller of admission and the place the acquire happens,
// in that order, which is not a cycle: it calls admission BEFORE the acquire and
// admission never calls back.
//
// The assertion that the other questions are answered elsewhere is AUDITED
// rather than assumed — an empty cell below is a finding, and not one that
// delegating the mouth would have fixed. Cells a caller genuinely leaves empty
// are DECLARED as a skip set rather than left to be inferred from missing code.
//
// THE TABLE HAS NO EMPTY CELLS LEFT. Both were closed the same way, two batches
// apart, and the shape was the same both times: the skip was never justified by
// "this caller does not need the answer", it was justified by "a refusal here
// would have nothing to release it". Window 2 built the releaser for the held-bin
// caller and then stopped skipping; the A batch did the same for the complex one,
// where the releaser is a service dig that does not consume the demand. A skip
// resting on a missing releaser is a note with an expiry, and both expired.
//
//	QUESTION      | FRESH-BIN plain order        | HELD-BIN plain order
//	--------------|-----------------------------|---------------------------
//	dig exclusion | admission, via admitLanes,   | SAME — dispatchHeldBin calls
//	              | plus admitMouth in the tx    | admitLanes too
//	ordering      | laneEntryCause, asked by the | SAME — one gate answers it
//	              | gate (gateEntryVerdict). An  | for both callers
//	              | unmarked lane orders nothing |
//	presence      | HOLD B, both halves — the    | same
//	              | order takes an occupancy row |
//	              | at the create seam and reads |
//	              | everyone else's (the         |
//	              | unification). The mouth row  |
//	              | is still taken where the     |
//	              | group configures one.        |
//	reachability  | finder Tier 6 (EMPTY intent  | ADMISSION asks it — window 2
//	              | only, source_finder.go:730)  | (3326c1bb) gave the refusal a
//	              | / NGRP resolver raising      | releaser first
//	              | BuriedError                  | (digForBuriedHeldBin), then
//	              | (group_resolver.go:241)      | skipsForEntry stopped skipping
//
// TWO THINGS THAT CELL TABLE SAYS OUT LOUD, both reached here from a different
// direction than they were first found, which is mild evidence they are real:
//
//  1. THE EMPTY CELL. An order already holding a bin never calls the finder — it
//     is structurally exempt from looking — so on a `none` or `mouth` group a
//     held bin that became buried since it was claimed dispatches a robot to a
//     slot behind another bin. `none` is what both plants run. It is OPEN: the
//     fix changes what happens on the floor, so it is a unit of its own rather
//     than something a convergence commit settles.
//
//  2. PRESENCE WAS TWO MECHANISMS THAT COULD NOT SEE EACH OTHER — CLOSED by
//     the unification. A plain order took a mouth row and never a Hold B
//     occupancy row; a compound leg took occupancy and never a mouth row of its
//     own. So this function's occupancy read could not see a plain order working
//     a lane, and admitMouth could not see a compound leg. What separated the two
//     classes was the PARENT's dig row — one row carrying a distinction nothing
//     stated anywhere else.
//
//     Plain orders now take occupancy at the fleet-create seam (dispatcher.go)
//     and at a gated tail append (lane_gate_dispatch.go), so ONE mechanism now
//     records every order's presence and this read sees all of it. The dig row is
//     back to meaning what it says rather than doubling as a class separator.
//
//     Still true, and worth keeping: admitMouth cannot see a compound leg. That
//     is the MOUTH's business (mode-sharing), not presence, and it stays with
//     the acquire where it belongs. (This said "gated on lane_enforcement".
//     There is no such property — it was deleted with its type, constants and
//     reader, having never been set at either plant. The mouth is universal now,
//     which makes the sentence wrong twice over.)
//
// ── FAIL-CLOSED IS A PROPERTY OF THE TYPE, NOT OF THE CALLER ──────────────
//
// The zero GateVerdict is REFUSED. Every arm that cannot determine an
// answer returns that zero value alongside its error, so a caller that ignores
// the error — or forgets to assign the verdict at all — still refuses.
//
// This is the one thing the sites this replaced got wrong, and it is why the
// shape is a struct rather than a bare bool. The pre-dispatch tiered gate
// (deleted, lane_entry.go) returned `park=false, err` on an unreadable lane,
// which reads as ADMIT at a glance and was correct only because both scanner
// call sites happened to check err first. The safety lived in every caller's
// discipline and a new caller inherits none of it. Here the dangerous reading
// is unreachable instead of merely discouraged — the same move as
// orders.open_for_children, where the safe state is the one you get by
// forgetting.
// ── THE SHAPE IS SHARED; THE QUESTIONS ARE NOT ───────────────────────────
//
// GateVerdict is named for the lane gate rather than for admission because the
// ORDERING decision (laneEntryCause, lane_entry.go) returns it too. Both are
// "a lane-gate verdict with a cause, whose zero value refuses", and both have an
// undetermined arm that must not read as a yes.
//
// Sharing the shape does NOT merge the decisions, and the name is deliberate
// about that: an `admissionVerdict` coming back from the ordering gate would
// tell a reader that ordering IS admission, which is the one confusion the
// boundary above exists to prevent. The questions stay apart because they are
// different FUNCTIONS returning different causes; only the envelope is common.
//
// It is for decisions that can be UNDETERMINED. classifyLaneEntry stays on a
// bare QueueCause because it is pure and total — it cannot fail, so its "" means
// a definite admit rather than an unanswered question, and wrapping it would
// invent a failure mode it does not have.
type GateVerdict struct {
	admitted bool
	cause    QueueCause
	lane     string
}

// Admitted reports whether the move may proceed. False on the zero value.
func (v GateVerdict) Admitted() bool { return v.admitted }

// Cause is the engineer-facing tag for a refusal, and "" when admitted.
func (v GateVerdict) Cause() QueueCause { return v.cause }

// Lane names the lane that refused, for the operator's "Waiting for a slot at
// ‹lane›" sentence. "" when admitted, and "" on a refusal whose decider had no
// single lane to name (the ordering gate).
//
// It rides on the verdict rather than on a second return value because the
// caller that needs it — the plain entry path, whose queue sentence names the
// contended lane — used to get it from a bespoke third return value on a
// bespoke dig-only function. Folding that function into admission had to keep
// the name reachable, and a refusal that cannot say WHERE is a worse refusal.
func (v GateVerdict) Lane() string { return v.lane }

// Admitted, Refused and RefusedAt are the only ways to build a verdict, so
// "admitted" is never spelled as a bare struct literal at a call site where a
// future field would silently default. The zero value stays a valid
// refusal-without-cause, which is what makes forgetting safe.
//
// Exported because the fulfillment Dispatcher interface returns this type, so
// anything implementing that interface — including its test doubles — has to be
// able to build one. That is a requirement of the boundary, not a concession.
func Admitted() GateVerdict            { return GateVerdict{admitted: true} }
func Refused(c QueueCause) GateVerdict { return GateVerdict{cause: c} }

// RefusedAt is Refused plus the lane that did the refusing.
func RefusedAt(c QueueCause, laneName string) GateVerdict {
	return GateVerdict{cause: c, lane: laneName}
}

// admissionSituation is what admission is asked ABOUT.
//
// A bundle rather than (bin, lane) so the decision can gain inputs without
// touching every call site — §5's requirement, and the fold will exercise it:
// a per-move decision needs a destination that does not exist on an order row
// at all, and a compound parent's sealedness, neither of which is derivable
// from a bin and a lane.
//
// Nil source or destination is legitimate and means "this move does not touch
// one" — a retrieve to a line node has no destination lane. It does NOT mean
// unknown; a caller that could not resolve a node must not reach here.
type admissionSituation struct {
	// order is the order that wants to move. Its identity matters, not just its
	// id: the dig exemption is resolved through laneOwnerFor, which routes a
	// compound child's question to its parent.
	order *orders.Order
	// sourceNode is where the move PICKS, nil when it picks nowhere gated.
	sourceNode *nodes.Node
	// destNode is where the move PLACES, nil when it places nowhere gated.
	destNode *nodes.Node
	// skip names the questions THIS CALLER DOES NOT ASK TODAY. See
	// admissionSkips — the zero value skips nothing.
	skip admissionSkips
}

// admissionSkips names physical questions a caller does not ask.
//
// ── WHY SKIPS AND NOT "ASKS" ──────────────────────────────────────────────
//
// The zero value asks EVERYTHING. That is the same discipline as GateVerdict
// above, for the same reason: a caller that forgets this field gets the
// conservative answer, not a silent unconditional admit. Spelled as an "asks"
// set, forgetting would mean asking nothing, and a new lane-entry path would
// fail open — the exact shape this file exists to make unreachable.
//
// ── WHY THE FIELD EXISTS AT ALL ───────────────────────────────────────────
//
// Convergence moves code, not questions (plan §9.5). Before it, three sites
// asked three different subsets of the same three physical questions in three
// spellings; after it there is one implementation of each question and one
// function composing them, and each caller's subset is DECLARED instead of
// being an emergent property of which lines somebody wrote.
//
// ── THE AUDIT RULE ────────────────────────────────────────────────────────
//
// A caller may skip a physical question ONLY IF that question is answered
// elsewhere on its path, AT A NAMED LINE. A skip with no line dies. Every
// surviving entry below carries its justification; adding one without a line is
// the thing this rule exists to stop.
//
// A second, harder condition learned while applying it: a question is not
// "answered" by refusing — a refusal with no NAMED RELEASER is a wedge, and
// trading a floor failure for a permanent park is not an improvement. Where
// that bites, it is recorded rather than papered over.
//
// THE DIG QUESTION IS NOT SKIPPABLE and deliberately has no field: every caller
// asks it, it is mode-independent, and a lane being dug is being dug whoever is
// asking.
// THE UNCONDITIONAL OCCUPANCY SKIP IS GONE. It had a field and no setter: the
// unification deleted the last caller that asked for it (skipsForPlainEntry) and
// left the field behind, so `!s.skip.occupancy` was a constant true guarding a
// question nobody could turn off. Keeping it would have offered the next caller
// the one skip the unification exists to make unavailable — and offered it with
// no audit line to write, since there is no answer to point at. A caller whose
// dispatch genuinely does not enter the lane says entryWhenGated, which is
// conditional on the lane and carries its justification.
type admissionSkips struct {
	reachability bool
	// entryWhenGated defers the WHOLE in-lane decision to the gate, on a MARKED
	// lane and only there. It is a statement about the CALLER's moment, not about
	// any one question.
	//
	// A caller sets it when its dispatch does not enter the lane: on a gated
	// group every lane-bound order is created unsealed ending at the lane's WAIT
	// POINT, outside the corridor. The robot drives, does all the pre-lane work
	// in its plan, and dwells. Entry is decided later, at the tail append, by
	// gateEntryVerdict / laneGateRetrieveCause — which ask everything and skip
	// nothing.
	//
	// IT USED TO DEFER ONLY OCCUPANCY, and that was half a rule. The dig arm ran
	// first and unconditionally, so a gated order whose lane was being dug was
	// refused BEFORE dispatch — parked whole, pre-lane work and all. That is the
	// disposition the gate exists to replace: "a third party's dig holds the lane,
	// so the retrieve pre-positions and dwells" is what the gate tests assert
	// about exactly this situation, and the splice's whole premise is that an
	// order does all the work it can before it waits. Refusing at dispatch threw
	// both away, and it re-asked at the gate anyway.
	//
	// If the dispatch does not enter the lane, NONE of the in-lane questions are
	// about it. Same question, asked once, at the moment it is about.
	//
	// It is narrow on purpose. Only a MARK moves the entry moment; an unmarked
	// lane has no wait point and its dispatch drives straight in, so deferring
	// there would mean nobody asks at all. Pinned by
	// TestQuestionSet_PlainEntryStillAsksOccupancyOnAnUnmarkedLane.
	entryWhenGated bool
}

// skipsForPlainEntry — what AcquireLanesForOrder skips, and why.
//
// ── occupancy: DELETED. This is THE UNIFICATION. ──────────────────────────
//
// It had no line. The claim it rested on — that a plain order's presence is
// carried by its MOUTH row — fails on the configuration both plants actually
// run: on `none`, resolveOrderLaneHolds yields no holds at all, so nothing
// recorded a plain order's presence and nothing read anyone else's. A plain
// order drove into a lane a reshuffle leg was physically inside.
//
// Deleting it is only half the question, so the other half went in with it:
// plain orders now TAKE occupancy at the moment Core sends them into the lane
// (dispatcher.go, after the handover; lane_gate_dispatch.go appendGateTail for
// a gated entry), and release on placement (wiring_block_completed.go) and on
// terminal (store/orders.go TerminalizeOrder → reservations.ReleaseByOrder).
// Asking "is anyone inside" is meaningless while the asker never appears in the
// answer.
//
// Named releaser for the new park: the fulfillment scanner already re-runs on
// the whole lane-clearing trigger set (wiring.go — EventBinUpdated,
// EventOrderCompleted/Cancelled/Failed/Skipped, EventBinEnteredTransit, and
// EventBlockCompleted registered AFTER handleBlockCompleted so the scan observes
// the released row). The ticker is a backstop, not the mechanism.
//
// ── entryWhenGated: the unification's OTHER half, on a gated lane ──────
//
// The unification's rule is that the ask and the take happen at the SAME moment
// — "asking is anyone inside is meaningless while the asker never appears in the
// answer" cuts both ways, and a read taken at a different moment from the write
// is the same defect wearing the other shoe.
//
// On a gated lane the TAKE moved: appendGateTail takes occupancy at
// the tail append, not at the create, because a gated create sends the robot to
// the wait point OUTSIDE the corridor and a row taken there would declare a robot
// present in a lane it is deliberately parked next to
// (lane_gate_dispatch.go:495-512). The ASK did not follow it, and that is the
// defect this flag closes: this path refused a plain order at dispatch for a
// corridor it was never about to enter, which puts the lane's wait back on the
// PRESS, which is the one outcome the gate exists to prevent: a lane's
// congestion must land on a robot, never on a press (see
// skipsForGatedStoreEntry).
//
// THE LINE THAT JUSTIFIES THE SKIP: gateEntryVerdict (lane_gate_release.go),
// asked at both moments a gated order can be let in — the valve's immediate check
// at create time and every evaluator pass afterwards — with a skip set that skips
// nothing. Pinned by TestGatePair_SameOriginPairSerializesAtTheMouth (one tail
// per lane-clear) and TestGatePair_NeitherPartnerIsHeldBeforeTheFleetCreate (the
// pair is not held at the press).
//
// NOT the dig, and not reachability. A dig is a long exclusive hold and whether a
// gate should park a robot through one is a real design question the ruling does
// not answer; burial is a fact about a BIN, and the held-bin path turns it into a
// dig here rather than carrying it to the gate (window 3, plan §12.10). Only
// occupancy has both a moved take and a ruling.
//
// ── reachability: KEPT, and the justification is PARTIAL. ─────────────────
//
// Answered for a FRESH-BIN order, two mechanisms, both real:
//
//   - EMPTY intent → the finder's post-find accessibility check,
//     source_finder.go:730-763, which returns OutcomeReshuffle with a
//     BuriedError; the scanner turns that into PlanBuriedReshuffle
//     (fulfillment/scanner.go:308,335). That is the named releaser — a dig gets
//     planned, and the lane clears.
//   - FULL retrieve from an NGRP source → binresolver/group_resolver.go:241
//     raises BuriedError, same disposition.
//
// IT WAS NOT ANSWERED for a HELD-BIN order, AND THAT GAP IS CLOSED — window 2,
// `3326c1bb`. Stated in the past tense because the shape of the fix is the
// reason this skip is now scoped rather than blanket.
//
// The gap was real: dispatchHeldBin reuses a bin claimed on an earlier tick and
// never calls the finder, so a later store can bury it in between and nothing
// looked. It could not be closed by refusing here, because a refusal needs a
// releaser and PlanBuriedReshuffle was wired to the FINDER's outcome — an order
// parked on burial with no finder in its path would never move again, which is
// a worse floor failure than driving to a slot it cannot reach.
//
// So the releaser was built first. fulfillment/scanner.go digForBuriedHeldBin
// routes a held-bin burial into reshuffle planning directly, which is what made
// the refusal safe; EntryKind below then splits the two plain callers so the
// held-bin one asks the question and the fresh-bin one keeps the skip its finder
// has already answered. Neither half works alone, which is why the skip stayed
// blanket for as long as it did.
//
// THIS SKIP SET IS NOW THE FRESH-BIN CALLER'S ONLY. See skipsForEntry.
var skipsForPlainEntry = admissionSkips{reachability: true, entryWhenGated: true}

// EntryKind names WHICH plain-entry caller is asking, because the two answer the
// reachability question differently and only one of them has an answer.
//
// This is the audit rule applied rather than bypassed: the skip's justification
// was never "plain orders don't need reachability", it was "the FINDER answered
// it" — and that is true of exactly one of the two callers. Making the caller
// declare itself is what lets the justified skip stay and the unjustified one go,
// instead of one set covering both and being wrong for half of them.
type EntryKind int

const (
	// EntryFreshBin is the caller that just resolved its source through the
	// finder this tick (fulfillment/scanner.go tryFulfill). Reachability is
	// answered there: the finder's post-find accessibility check for an empty
	// intent (source_finder.go), or the NGRP resolver raising BuriedError for a
	// full retrieve (binresolver/group_resolver.go). Both route to
	// PlanBuriedReshuffle, so the answer comes with a dig.
	EntryFreshBin EntryKind = iota
	// EntryHeldBin is the caller that reuses a bin claimed on an EARLIER tick
	// and never calls the finder (fulfillment/scanner.go dispatchHeldBin). Nothing
	// looked, so nothing answered, and a store can bury the held bin in between.
	//
	// NARROWED — the selector DOES guard the hard-claim case now. This said the
	// slot selector has no guard against placing in front of a claimed bin at
	// all, and that was true when it was written. findStoreSlot's burial clause
	// (store/nodes/lanes.go) refuses a candidate that sits shallower in the same
	// lane than a bin claimed by a live order, matched on either spelling of
	// ownership and filtered on the holder being non-terminal.
	//
	// What is still unguarded is the SOFT hold: a bin promised by a pending
	// reservation rather than hard-claimed has no claimed_by and no holder row
	// to join, so nothing shallower is refused on its account. That is the
	// window this entry kind exists for, and it is narrower than the sentence
	// used to claim.
	EntryHeldBin
)

// skipsForEntry returns the skip set for a plain-entry caller.
func skipsForEntry(kind EntryKind) admissionSkips {
	if kind == EntryHeldBin {
		// The held-bin caller asks every physical question, including the
		// reachability one the fresh caller may skip.
		//
		// entryWhenGated is the one thing it shares with the fresh caller, and
		// deliberately: it is not a claim about what this caller knows, it is a
		// claim about WHERE THIS PATH'S DISPATCH ENDS. Both plain callers go
		// through dispatchToFleetCore, so on a gated lane both stop at the wait
		// point and both defer entry to the gate.
		return admissionSkips{entryWhenGated: true}
	}
	return skipsForPlainEntry
}

// skipsForGatedStoreEntry is what the valve and the evaluator ask before letting
// a gate-staged STORE into its lane.
//
// dig: ASKED, and this is the gap the transform exposed. The store arm used to
// run the ORDERING classifier (laneEntryCause, tiers 1-3) and nothing else, which
// asks whose turn it is and never whether the lane is open at all. That was
// survivable while only PLAIN stores reached the valve: they had already been
// through AcquireLanesForOrder at dispatch, which asks the dig. A COMPLEX order
// does not go through the scanner's admit at all, so once the splice let complex
// orders reach the valve there was nothing between a dig and a robot. "A dig
// excludes everything" is settled; this is where the store path started saying it.
//
// reachability: not applicable. It is a pickup-end question and a store's lane
// node is its DROPOFF; admitLane already returns early for a non-source end.
//
// occupancy: ASKED. THE SKIP IS DEAD.
//
// Hold B means strictly ONE robot inside a gated lane, pair or no pair.
// The Tier-1 conflict this skip used to carry resolves by MOVING what Tier 1
// protects, not by weakening either mechanism. Tier 1 co-releases same-origin
// partners because depth-gating them adds latency to the press; that value moves
// to where it costs nothing — the pair is still DISPATCHED together, both robots
// en route, so the press-side swap never waits on the lane. What
// serializes is their ENTRY, at the lane mouth, on a robot already standing at a
// gate point. The wait lands on a robot, never on the press.
//
// Concretely, in one evaluator pass: the first partner is admitted, its tail is
// appended, and appendGateTail takes its occupancy row (lane_gate_dispatch.go) —
// so the second partner, evaluated against a freshly read view later in the same
// loop, is refused HERE with CauseLaneOccupied and STAYS a candidate. It goes in
// on the next lane-clearing event. One tail per lane-clear, which is what
// single-file means.
//
// The releaser is named and is the same one the deleted retrieve skip rests on:
// the evaluator re-runs on every lane-clearing event (engine/wiring_lane_gate.go)
// and the refused partner keeps dwelling at its gate point until the lane is free.
// A gate wait is what this refusal is FOR; it is not a park with nothing to clear
// it.
//
// It also sets the convoy default (plan §12.5b): one-inside is the rule, and any
// two-in-a-lane choreography is an explicit future exception rather than a
// leniency the gate started with.
var skipsForGatedStoreEntry = admissionSkips{}

// skipsForGateStagedRetrieve WAS HERE AND IS DELETED — its occupancy skip was
// the only entry, so laneGateRetrieveCause now asks everything.
//
// Its justification was that occupancy stood in for nothing missing: the mouth
// gate serialises same-mode sharers, and the only orders that HELD occupancy
// rows were compound legs — whose parent holds the dig, which this caller
// already refuses against. Both halves were true when written.
//
// THE UNIFICATION KILLED IT. Plain orders now take occupancy rows, and a plain
// store inside a lane holds no dig and (on `none`) no mouth row either. So a
// gate-staged retrieve skipping occupancy would be released — a robot let into a
// lane another robot is placing in, which is the exact collision Hold B exists
// to prevent, arrived at through the release path instead of the dispatch path.
//
// Deleting the skip is safe in the way the reachability skip above is not: this
// caller's refusal HAS a named releaser by construction. The evaluator re-runs
// on every lane-clearing event (engine/wiring_lane_gate.go) and the order simply
// keeps dwelling at its gate point until the lane is free, which is what a gate
// wait is for. The retrieve does not block itself either — it takes its own
// occupancy row only when its tail is appended, which is after this verdict.

// skipsForComplexEntry is what a coordinated (complex) order asks before its
// fleet create.
//
// ── IT ASKED NOTHING AT ALL, and that is what this closes ─────────────────
//
// The boundary map above lists three delegates and a complex order is not among
// them: the scanner branches on IsCoordinated to DispatchPreparedComplex, which
// never calls AcquireLanesForOrder, and the valve only stands in front of a
// GATED lane. Both plants run every lane ungated, so in practice a complex order
// — the changeover swaps, which are most of the plant's lane traffic — reached
// the fleet with nothing between it and a corridor. The dig gap was written down
// beside skipsForGatedStoreEntry ("nothing between a dig and a robot") and
// closed for the gated arm only; the ungated arm is the same hole, on the
// population that actually exists.
//
// dig: ASKED. A dig excludes everything, and it is mode-independent.
//
// occupancy: ASKED, AND ANSWERED INTO. A complex order takes its occupancy rows
// at the create seam like every other order — commitToFleet, over the pre-wait
// segment's nodes — so this read sees everyone else and everyone else sees it.
//
// THIS SENTENCE WAS FALSE FOR AS LONG AS IT EXISTED, and it is worth saying so
// rather than quietly correcting it. The unification wired the READ and this
// paragraph described the write as done; the ungated arm of dispatchComplexToFleet
// never took a row. So the arm carrying the bulk of both plants' lane traffic
// asked the question and never appeared in the answer, and the collision that
// allowed was invisible to every checker — they all read these rows. A comment
// asserting an invariant that nothing enforces is worse than no comment: it is
// where the next person stops looking.
//
// entryWhenGated: SET, same claim as both plain callers — a gated create
// stops at the wait point, so the entry decision belongs to the tail append and
// gateEntryVerdict asks it there with a skip set that skips nothing.
//
// reachability: ASKED, since the A batch. It used to be skipped, and the skip's
// justification was the RELEASER condition rather than the line condition —
// which is exactly the shape that becomes wrong when the releaser changes.
//
// PARTIALLY ANSWERED ELSEWHERE, and still is: an NGRP-sourced pickup gets it
// from the resolver, which raises BuriedError (binresolver/group_resolver.go:241)
// and is routed to the burial handler by both the NGRP re-resolve and the supply
// widen. That is a real answer with a real dig behind it.
//
// NOT ANSWERED for a pickup already resolved to a concrete lane slot: those come
// from the allocator's findAvailableForNeed (allocator.go), which reads a single
// node and never asks what is in front of it. That was the hole, and it was left
// open deliberately: the complex dig used to be wired to a FINDER/resolver
// outcome rather than to an admission verdict, so a lane-target-buried refusal
// raised HERE would have parked the order with nothing to unbury it — for ever.
// Trading "drives to a slot it cannot reach" for "never moves again" is not an
// improvement, so the note said: close it by giving the refusal a dig, or accept
// it. Not by deleting the line alone.
//
// The refusal now HAS a dig, which is what makes asking safe: admitComplexLanes
// can refuse on an unreachable pickup AND ask for the corridor to be opened.
// Both halves landed together; neither works alone.
//
// ── AMENDED BY §R.91, AND THIS PARAGRAPH SAID OTHERWISE UNTIL §R.98 ───────
//
// It read "a dig is a service to a lane and is proposed without consuming the
// demand (proposeLaneClearDig)". That was the two-shape rule, and §R.91 replaced
// it: the demand that causes a dig BECOMES its parent, wears `reshuffling`, and
// resumes through `queued` into its own lifecycle. The demand is consumed — that
// is the unification. What survives unchanged is the property this paragraph was
// really relying on: the refusal has somewhere to send the work, so raising it
// does not park the order forever.
//
// The other half survives too, restated as a status rule by round 2: a dig is
// owned by the demand that caused it UNLESS a vehicle is already committed, and
// then it serves the lane.
var skipsForComplexEntry = admissionSkips{entryWhenGated: true}

// admitPlan asks admission of every lane-touching step in a resolved plan.
//
// The single-source/single-destination shape admit() takes fits an order with two
// endpoints. A complex plan has many, and its lane entry can be interior — a
// robot picks at a press, drops in a lane, picks from another node. Walking the
// steps is the only way to ask about the moves the plan actually makes.
//
// It composes admitLane rather than reimplementing anything: the physical
// questions stay in one function, which is the property the convergence is for.
// A pickup step asks the pickup-end question set, a dropoff step the placing one,
// exactly as the two-endpoint form does.
//
// FIRST REFUSAL WINS, and blank/unresolvable nodes are skipped rather than
// refused — a step with no node yet is placeForDedicatedLoader's to fill, and a
// name that does not resolve to a Core node is the claim path's to surface. A
// lane that resolves but cannot be READ still refuses, because that is
// admitLane's own fail-closed arm and it must not be softened by being reached
// from here.
// planNodes resolves the lane-relevant nodes a plan's steps touch — the WRITE
// side's twin of admitPlan's walk, and deliberately the same walk.
//
// admitPlan asks each step's node "may I enter"; this collects the same nodes so
// the caller can record that it did. The two questions must be asked of the same
// set or an order can be admitted to a lane it then fails to register itself in,
// which is exactly the asymmetry that made a complex order invisible to everyone
// else. Same predicate (pickup/dropoff, non-blank, resolvable), same silent skip
// of what cannot be resolved — a node admitPlan could not classify is a node this
// cannot take a row on either.
func (d *Dispatcher) planNodes(steps []resolvedStep) []*nodes.Node {
	var out []*nodes.Node
	for _, step := range steps {
		if step.Action != protocol.ActionPickup && step.Action != protocol.ActionDropoff {
			continue
		}
		if step.Node == "" {
			continue
		}
		node, err := d.db.GetNodeByDotName(step.Node)
		if err != nil || node == nil {
			continue
		}
		out = append(out, node)
	}
	return out
}

func (d *Dispatcher) admitPlan(order *orders.Order, steps []resolvedStep, skip admissionSkips) (GateVerdict, error) {
	s := admissionSituation{order: order, skip: skip}
	for _, step := range steps {
		isSource := step.Action == protocol.ActionPickup
		if !isSource && step.Action != protocol.ActionDropoff {
			continue
		}
		if step.Node == "" {
			continue
		}
		node, err := d.db.GetNodeByDotName(step.Node)
		if err != nil || node == nil {
			continue
		}
		v, err := d.admitLane(s, node, isSource)
		if err != nil || !v.Admitted() {
			return v, err
		}
	}
	return Admitted(), nil
}

// entryDeferredToGate reports whether this caller's dispatch stops OUTSIDE the
// lane — the marked-lane case, where the create ends at the wait point and only
// the tail append puts a robot in the corridor.
//
// False unless the caller declared entryWhenGated, so a caller that forgets
// the field asks the question, which is the same forgetting-is-safe discipline as
// the rest of this file. False for a lane with no group: an ungated lane cannot
// defer anything to a gate that is not there.
//
// laneIsGated is the shared half. The OTHER decision that turns on "does the gate
// own this lane's entry" is Hold B's node door (TakeLaneOccupancy, lane_gate.go),
// which skips a gated lane for the same reason this skips its questions: the
// robot is at the mark, and the append owns both the entry and the row. One
// spelling, two decisions — what differs is only this caller's declaration, which
// is a statement about the CALLER's moment rather than about the lane.
func (d *Dispatcher) entryDeferredToGate(skip admissionSkips, lane *nodes.Node) bool {
	if !skip.entryWhenGated || lane.ParentID == nil {
		return false
	}
	return d.laneIsGated(lane.ID)
}

// admit answers whether this move may happen now.
//
// The checks run source-lane-first, then destination, and the FIRST refusal is
// returned — the causes are for an engineer reading why an order waited, and the
// first thing in the way is the useful answer. Order between the checks on one
// lane is deliberate and runs cheapest-and-most-decisive first: a foreign dig
// excludes everything, so there is no point asking about occupancy or
// reachability underneath one.
//
// A lane whose group Core does not own contributes nothing, so an unconfigured
// plant resolves two nodes and admits — the gate stays a no-op where it is off.
func (d *Dispatcher) admit(s admissionSituation) (GateVerdict, error) {
	if s.order == nil {
		// Not a refusal with a cause — a caller bug. There is no order to park
		// and no queue row to write a cause onto.
		return GateVerdict{}, fmt.Errorf("admission: no order in situation")
	}

	// Source first: a move that cannot pick has nothing to place.
	if v, err := d.admitLane(s, s.sourceNode, true); err != nil || !v.Admitted() {
		return v, err
	}
	return d.admitLane(s, s.destNode, false)
}

// admitLane is the per-lane half. isSource selects the reachability check, which
// only means anything at the pickup end: a store's target slot being buried is
// the placer's problem and a different question.
//
// s.skip is honoured per question. A skipped question is not asked and its READ
// IS NOT ISSUED — the plain entry path is the highest-traffic caller in the
// system and must not start paying for two queries whose answers it discards.
func (d *Dispatcher) admitLane(s admissionSituation, node *nodes.Node, isSource bool) (GateVerdict, error) {
	if node == nil {
		return Admitted(), nil // this move does not touch a lane here
	}

	// RESOLVED, NOT SKIPPED. lanesFor logs a LaneForNode failure and continues,
	// which means an unresolvable lane contributes no checks and the move is
	// admitted — the exact fail-open shape this type exists to make unreachable.
	// Admission resolves it itself and propagates.
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil {
		return GateVerdict{}, fmt.Errorf("admission: resolve lane for node %d: %w", node.ID, err)
	}
	if lane == nil {
		return Admitted(), nil // not a lane slot — nothing here to arbitrate
	}
	// NO ENFORCEMENT-MODE GATE, and that is a correction rather than an omission.
	//
	// The questions below are PHYSICAL: is a robot inside this corridor, does
	// another reshuffle own it, can this bin be reached. A single-file lane is
	// single-file whether or not Core arbitrates its mouth, and the facts these
	// read are written unconditionally — TakeLaneOccupancy resolves lanes through
	// lanesFor with no mode check, and both reshuffle planners take and test the
	// dig lock without consulting one either.
	//
	// MOUTH MODE-SHARING (inbound/outbound compatibility, depth sequencing) is
	// AcquireLanesForOrder's business and stays there. It is a separate question,
	// not a switch over this one — and this paragraph used to say a
	// `lane_enforcement` property "selects who owns" it and "is correctly gated
	// there", which is false in both halves: the property was deleted with its
	// type, its constants and its reader, having never been set on any node at
	// either plant, and the mouth is now taken on every lane whether or not
	// anyone drew a mark on the map.
	//
	// The warning the paragraph existed to give survives intact, because it was
	// never about the property: gating the PHYSICAL questions on any
	// configuration would mean an ordinary group silently stopped serialising
	// compound legs — occupancy rows still written, never read, two robots into
	// one corridor. This version briefly did exactly that, and its own test
	// asserted it was right.
	//
	// THE ONE MODE READ BELOW IS NOT THAT, and the distinction is the whole of
	// entryWhenGated: it does not gate a QUESTION on a mode, it asks whether
	// THIS CALLER's dispatch is the moment the lane is entered. On a
	// gated lane it is not — the create stops at the wait point and the
	// gate decides — so a caller that says so is deferring the questions to the
	// moment they are about, not dropping them. Only a caller that declares the
	// flag can reach the read at all; every other caller's answer is
	// mode-independent, as above.
	//
	// A lane with no GROUP is still a lane, so lane.ParentID being nil is not an
	// admit either.

	// 0. IS THIS CALLER EVEN ENTERING THE LANE? On a gated lane a declaring
	// caller's dispatch stops at the wait point, so none of the questions below
	// are about it — not the dig, not occupancy, not reachability. They are asked
	// at the tail append instead, by a caller that skips nothing.
	//
	// BEFORE the dig arm, and that placement is the fix rather than a
	// micro-optimisation. With the dig asked first and unconditionally, a gated
	// order whose lane was being dug was refused at DISPATCH — parked whole, its
	// pre-lane work never sent — which is precisely the disposition the gate
	// exists to replace, and it was re-asked at the gate a moment later anyway.
	if d.entryDeferredToGate(s.skip, lane) {
		return Admitted(), nil
	}

	// 1. A FOREIGN DIG EXCLUDES EVERYTHING. DigOwner rather than IsLocked
	// because this arm can propagate: IsLocked answers "held" on an unreadable
	// row, which is the right disposition for a caller that cannot report an
	// error and the wrong one here, where a fabricated answer would replace a
	// correct refusal-with-reason by a guess.
	//
	// ownsDig, not isOwnDigLeg, and that is the convergence's one behavioural
	// consolidation — see ownsDig (lane_gate.go) for why the owner arm is the
	// one isOwnDigLeg could not express and what has to be revisited if a
	// digger is ever given a pre-position.
	digOwner, err := d.laneLock.DigOwner(lane.ID)
	if err != nil {
		return GateVerdict{}, fmt.Errorf("admission: dig owner for lane %d: %w", lane.ID, err)
	}
	if digOwner != 0 && !d.ownsDig(s.order.ID, digOwner) {
		// ── THE REFUSAL IS DECIDED; ONLY ITS NAME IS CHOSEN HERE (§R.101) ──
		//
		// A foreign dig-mode row excludes this order whichever KIND it is, and
		// that stays true: §R.101 rules that a demand owns the lane it sourced
		// from until the bin leaves by its mover, so gating on the excavation
		// read instead of DigOwner above would let a second order into a lane a
		// demand owns. The read below cannot admit anybody; it can only pick
		// between two words for a refusal already made.
		//
		// It is asked here rather than left to the caller because this arm is the
		// one that names the lane, and lane-dig-active was telling engineers to
		// go and find an excavation that a plain retrieve's source hold had
		// simply been misfiled as.
		//
		// A FAILED READ KEEPS THE OLDER NAME rather than failing admission. The
		// refusal is correct either way and the order parks either way; turning a
		// good refusal into an error because a forensic label could not be
		// resolved would trade a right answer for none. lane-dig-active is the
		// conservative choice of the two — it sends a reader looking for the
		// bigger thing.
		cause := CauseLaneDigActive
		excavator, xErr := d.laneLock.ExcavationOwner(lane.ID)
		switch {
		case xErr != nil:
			log.Printf("admission: could not tell whether lane %d's hold is an excavation: %v "+
				"(labelling the refusal %s)", lane.ID, xErr, CauseLaneDigActive)
		case excavator == 0:
			cause = CauseLaneHeldSource
		}
		return RefusedAt(cause, lane.Name), nil
	}

	// 2. SOMEBODY IS INSIDE. Hold B — the per-leg occupancy row, taken at
	// dispatch because Core's own decision to send is the entry moment. (A gated
	// caller has already returned at arm 0; its entry moment is the tail append.)
	occupants, err := reservations.OccupantsOf(d.db.DB, lane.ID)
	if err != nil {
		return GateVerdict{}, fmt.Errorf("admission: occupants of lane %d: %w", lane.ID, err)
	}
	for _, occ := range occupants {
		if occ != s.order.ID {
			return RefusedAt(CauseLaneOccupied, lane.Name), nil
		}
	}

	// 3. IS WHAT WE WANT ACTUALLY REACHABLE. Pickup end only.
	if !isSource || s.skip.reachability {
		return Admitted(), nil
	}
	target, _, err := d.pickupSlotNow(s.order, lane)
	switch {
	case errors.Is(err, ErrPickupNotInLane):
		// A DEFINITE NO, SO A REFUSAL — not an error. The bin this pickup is for
		// exists and is somewhere else, which is a fact about the plant that can
		// change, not a question Core failed to answer.
		//
		// It used to propagate as a bare error, and the difference is a wedge: an
		// error arm logs and skips the candidate, so the order carried no usable
		// cause, was never proposed for a heal dig, and — because a gate-staged
		// order is exempt from the abandon sweep — was bounded by nothing at all.
		// One order sat ten hours that way holding a bin, a robot and a lane.
		return RefusedAt(CauseGatePickupElsewhere, lane.Name), nil
	case err != nil:
		return GateVerdict{}, fmt.Errorf("admission: pickup slot for order %d in lane %d: %w",
			s.order.ID, lane.ID, err)
	}
	blockers, err := findBuriedBlockers(d.db, target.ID)
	if err != nil {
		return GateVerdict{}, fmt.Errorf("admission: blockers in front of slot %d: %w", target.ID, err)
	}
	if len(blockers) > 0 {
		return RefusedAt(CauseLaneTargetBuried, lane.Name), nil
	}
	return Admitted(), nil
}
