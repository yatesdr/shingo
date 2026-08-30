package dispatch

import (
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/store"
)

// QueueParams carries the values the operator-visible sentence is generated from.
// They are NOT persisted — the formatter consumes them at set-time to build the
// sentence (queue_reason) and then they are discarded. Nothing rebuilds the
// sentence later; the structured queue_code + queue_cause columns carry the
// analytic signal, and the sentence carries the human one.
//
// Every field here is READ by the formatter for at least one code. That is a
// standing rule, not an accident: the 2026-07-20 Springfield study found Lane
// and Sibling documented, populated by callers, and never read, so the operator
// was shown "Rearranging storage to reach this material" with no lane and
// "Waiting for partner robot" with no partner. If a field stops being read,
// delete it or render it — do not leave it populated and ignored.
//
// Field use is per code:
//   - QueueWaitingForMaterial: Payload + Kind + Partial + Group (+ Step)
//   - QueueWaitingForSlot:     Destination + BlockingBins/InboundOrders +
//     DestUnresolved (+ Step)
//   - QueueStorageRearranging: Lane + Payload + DigOrderID + HolderOrderID + StoppedOrderID (+ Step)
//   - QueueWaitingForPartner:  Sibling
//   - QueueFleetUnavailable:   none
type QueueParams struct {
	// Payload is the part code the order is waiting on material for. Empty for
	// an empty-carrier wait, and empty when the capacity shape could not be
	// classified (see QueueParams.Group and the unclassified note below).
	Payload string
	// Kind is "full" (a loaded bin of payload) or "empty" (an empty carrier).
	// Defaults to "full" when empty; only meaningful for QueueWaitingForMaterial.
	Kind string
	// Partial is true when the order holds part of a multi-bin set (a complex
	// "3 of 5" reserve). Rendered, because "waiting holding nothing" and
	// "waiting holding half the set" are different operator situations.
	Partial bool
	// Destination is the delivery node the order is waiting on a slot at.
	Destination string
	// Lane is the storage lane being rearranged (burial / reshuffle).
	Lane string
	// DigOrderID names the EXCAVATION this order is waiting on.
	//
	// ── WHY A WAIT NAMES THE DIG AND NOT JUST THE LANE ────────────────────
	//
	// "Rearranging lane LSD_01 to reach PANEL-B" is true and it is one word plus
	// a lookup. An operator watching a demand sit there has to go and find which
	// dig is running and whether it is the one that will release them — questions
	// on a different page, against a lane that may have had several digs on it
	// during the wait. The dig id is the join key for all of them.
	//
	// IT DOES NOT NAME THE SLOT BEING UNCOVERED, and that needs a decision to
	// change rather than a cleverer read: nothing persists what a dig is digging
	// toward. The plan's target slot is in-memory only, and an unbury step names
	// the BLOCKER's slot — so reading one would print the bin in the way.
	//
	// Optional. A wait with no identifiable dig — a lane held by
	// an ordinary order, a plan refused before any dig was proposed — renders
	// exactly as it did before, because a dig id we could not resolve must not be
	// invented. Zero is "not known", not "none".
	DigOrderID int64
	// StoppedOrderID names an order that has STOPPED and has to be resolved by a
	// person before this wait can end (§R.115). Set only with
	// CauseDigBlockerStopped.
	//
	// ── WHY THE ID IS IN THE SENTENCE ─────────────────────────────────────
	//
	// Every other wait here names a thing that is going to happen: a robot
	// arrives, a slot frees, a dig finishes. This one names a thing that will not
	// happen until somebody does it, so the sentence has to carry the one fact
	// that makes doing it possible — WHICH ORDER. "A bin in front of you is held
	// by an order that stopped" sends an operator hunting through a board; naming
	// the id makes it one row to open.
	//
	// Zero means not known, and then the sentence says less rather than inventing
	// a number — the same rule DigOrderID follows.
	StoppedOrderID int64
	// HolderOrderID names the demand that is AHEAD of this one on a bin — the
	// ranked take's winner (§7). Set only on a dig-blocker-promised park, where
	// the wait is on another order's turn rather than on any machine, so the
	// operator can look up who it is waiting behind.
	HolderOrderID int64
	// Sibling is the partner order's edge UUID in a two-robot swap.
	Sibling string

	// Group is the node group whose contents (not whose slots) the order is
	// short of — the supermarket that has no bin of the payload, NOT the
	// lineside delivery node. Naming the delivery node here is the F1 defect:
	// it sent operators to the wrong place.
	Group string
	// BlockingBins is how many bins physically occupy the destination. Set only
	// when that is what blocks the dropoff.
	BlockingBins int
	// InboundOrders is how many in-flight orders are already headed to the
	// destination. Set only when that is what blocks the dropoff.
	//
	// BlockingBins and InboundOrders are the F2 discriminator: "a bin is sitting
	// there" and "another order is on its way" need different operator responses
	// and used to render as the same sentence.
	InboundOrders int
	// Step is the zero-based step index of a multi-step (complex) order, and
	// HasStep says whether it is meaningful — step 0 is a real step, so the zero
	// value cannot carry that by itself.
	Step    int
	HasStep bool
	// DestUnresolved marks the destination node as unresolvable right now (a
	// lookup failure), rather than resolvable-but-full. Different problem,
	// different fix, so it gets its own sentence.
	DestUnresolved bool
	// Reserved marks a material wait where the group DOES hold what is wanted
	// and this asker may not have it — a strict maintained group the need is not
	// supported at.
	//
	// It exists because "waiting for an empty bin in PRESS-BUFFER" is a lie by
	// omission here. The empties are standing in that group, visible from where
	// the operator is reading the board, and the sentence sends them to look for
	// material that is already in front of them. The fix is not more material;
	// it is adding this process to the group's supported list, or sourcing from
	// somewhere else. Those are different actions, so it is a different sentence.
	Reserved bool
	// AtLevel marks a slot wait where the group is holding the number of empties
	// it was configured to hold, rather than being physically out of space.
	//
	// Same argument as Reserved, one code over. "Waiting for a slot at
	// PRESS-BUFFER" sends an operator to look for room, and there IS room — the
	// positions are free and spoken for by a number somebody typed. What ends
	// this wait is a carrier leaving OR the level being raised, and neither is
	// findable from a sentence about slots.
	AtLevel bool
}

// withHolder returns p with the holder named. A fluent setter rather than a
// field write at each site, because the holder is threaded onto params built
// elsewhere and the callers read better for it.
func (p QueueParams) withHolder(orderID int64) QueueParams {
	p.HolderOrderID = orderID
	return p
}

// FormatQueueSentence renders the operator-visible sentence for a queue code +
// parameters. This is the ONE place the wording lives: every producer passes a
// code + params, the sentence is generated here, and the caller writes
// sentence+code+cause together. Adding a code means handling it here (the
// exhaustiveness test walks AllQueueCodes through this function so an unhandled
// code fails the build, not silently renders empty).
//
// The sentence must never claim more than the params support. Where a value is
// absent the wording gets less specific rather than inventing a default — an
// unclassified capacity error reads "Waiting for material", not
// "Waiting for an empty bin", which is what it used to say.
//
// The wording is fixed, and a snapshot test pins the exact strings: these
// sentences are read on the floor, so changing one is a change to what an
// operator is told, not a copy edit.
func FormatQueueSentence(code protocol.QueueCode, p QueueParams) string {
	var s string
	switch code {
	case protocol.QueueWaitingForMaterial:
		s = materialSentence(p)
	case protocol.QueueWaitingForSlot:
		s = slotSentence(p)
	case protocol.QueueStorageRearranging:
		s = rearrangingSentence(p)
	case protocol.QueueWaitingForPartner:
		s = partnerSentence(p)
	case protocol.QueueFleetUnavailable:
		s = "Robot system not responding — retrying"
	default:
		return ""
	}
	return withStep(code, p, s)
}

// materialSentence covers the three material shapes: an empty carrier, a named
// payload, and an unclassified shortage. The last one used to fall through to
// "Waiting for an empty bin" because the branch tested Payload == "" alongside
// the empty kind — a full-bin wait with an unknown payload told the operator to
// go find an empty. It now says only what it knows.
func materialSentence(p QueueParams) string {
	var s string
	switch {
	case p.Kind == "empty":
		s = "Waiting for an empty bin"
	case p.Payload != "":
		s = fmt.Sprintf("Waiting for material: %s", p.Payload)
	default:
		s = "Waiting for material"
	}
	// Group, not Destination: the shortage is in the group being sourced FROM.
	if p.Group != "" {
		s += fmt.Sprintf(" in %s", p.Group)
	}
	// RESERVED REPLACES THE WHOLE SENTENCE rather than appending to it, because
	// the sentence above is not true here: the group is not short of anything.
	if p.Reserved {
		if p.Group != "" {
			s = fmt.Sprintf("%s is kept for other equipment — waiting for an empty from elsewhere", p.Group)
		} else {
			s = "That group's empties are kept for other equipment — waiting"
		}
	}
	if p.Partial {
		s += " — partial set already held"
	}
	return s
}

// slotSentence names the destination and, when known, WHY it is unavailable.
// The bin count and the inbound count are both computed at the capacity gate;
// carrying them here is what makes "go clear it" and "wait, one is coming"
// distinguishable without reading queue_cause (which no surface renders).
func slotSentence(p QueueParams) string {
	if p.DestUnresolved {
		if p.Destination == "" {
			return "Waiting on a destination that cannot be resolved right now"
		}
		return fmt.Sprintf("Waiting on destination %s — cannot be resolved right now", p.Destination)
	}
	// AT LEVEL IS NOT OUT OF ROOM, and it gets its own sentence for the reason
	// QueueParams.AtLevel gives: the positions are free.
	if p.AtLevel {
		if p.Destination != "" {
			return fmt.Sprintf("%s already holds the empties it is set to keep — waiting for one to leave",
				p.Destination)
		}
		return "That group already holds the empties it is set to keep — waiting for one to leave"
	}
	s := "Waiting for a slot"
	if p.Destination != "" {
		s += fmt.Sprintf(" at %s", p.Destination)
	}
	switch {
	case p.BlockingBins > 0:
		s += fmt.Sprintf(" — %s there now", plural(p.BlockingBins, "bin", "bins"))
	case p.InboundOrders > 0:
		s += fmt.Sprintf(" — %s already inbound", plural(p.InboundOrders, "order", "orders"))
	}
	return s
}

// rearrangingSentence reads Lane and Payload, both of which callers already
// pass. On a plant with many lanes, "storage is being rearranged" without
// naming the lane or the part is not actionable.
//
// AND IT NAMES THE DIG when the dig is known. The lane alone left the operator
// with one word and a lookup: which excavation, uncovering what, and is it the
// one that frees me. See QueueParams.DigOrderID.
func rearrangingSentence(p QueueParams) string {
	s := "Rearranging storage to reach this material"
	switch {
	case p.Lane != "" && p.Payload != "":
		s = fmt.Sprintf("Rearranging lane %s to reach %s", p.Lane, p.Payload)
	case p.Lane != "":
		s = fmt.Sprintf("Rearranging lane %s to reach this material", p.Lane)
	case p.Payload != "":
		s = fmt.Sprintf("Rearranging storage to reach %s", p.Payload)
	}
	return s + digClause(p) + holderClause(p) + stoppedOrderClause(p)
}

// holderClause names the demand that is ahead on the bin.
//
// The promised wait's whole distinction from the claimed one is that nothing is
// MOVING — the holder has a promise, not a robot — so "waiting for a lane to be
// rearranged" is true and useless on its own. The id is what makes it
// actionable: an operator can look up that order and see what it is waiting for
// in turn.
//
// Empty when no holder is named, so every other rearranging sentence is
// byte-identical to what it was.
func holderClause(p QueueParams) string {
	if p.HolderOrderID == 0 {
		return ""
	}
	return fmt.Sprintf(" — order %d is ahead of it on that bin", p.HolderOrderID)
}

// stoppedOrderClause is the one wait sentence that names a PERSON'S job.
//
// It is deliberately not phrased as a releaser the plant will supply, because it
// is not one: the bin in front cannot move until somebody resolves the order
// holding it (§R.115). The words are the floor's, not the code's — "has stopped"
// rather than "is non-terminal and outside the stall window", and "needs someone
// to sort it out" rather than a function name.
//
// Empty when no order is named, so every other rearranging sentence is
// byte-identical to what it was.
func stoppedOrderClause(p QueueParams) string {
	if p.StoppedOrderID == 0 {
		return ""
	}
	return fmt.Sprintf(" — order %d is holding the bin in front and has stopped; it needs someone "+
		"to sort it out", p.StoppedOrderID)
}

// digClause appends the excavation's identity to a rearranging sentence.
//
// Empty when no dig is known, so an ordinary lane-locked wait reads exactly as it
// did before any of this existed.
func digClause(p QueueParams) string {
	if p.DigOrderID == 0 {
		return ""
	}
	return fmt.Sprintf(" — dig %d is working this lane", p.DigOrderID)
}

// partnerSentence names the partner order. The pre-code free text said "swap:
// holding removal leg until supply sibling claims a bin" — it explained WHICH
// leg this is and what it is waiting for. Sibling was passed all along and
// never read.
func partnerSentence(p QueueParams) string {
	if p.Sibling == "" {
		return "Waiting for partner robot"
	}
	return fmt.Sprintf("Holding this leg until partner order %s secures a bin", shortRef(p.Sibling))
}

// withStep prefixes the failing step of a multi-step order. A five-step complex
// order that is blocked used to say only that it was blocked; the pre-code free
// text led with "step 0:" and named the leg. Fleet-unavailable is a whole-order
// condition, so it takes no step prefix.
func withStep(code protocol.QueueCode, p QueueParams, s string) string {
	if !p.HasStep || s == "" || code == protocol.QueueFleetUnavailable {
		return s
	}
	return fmt.Sprintf("Step %d: %s", p.Step, s)
}

// shortRef trims a UUID to its first segment — enough to correlate two legs of a
// swap on screen without spending a line of an operator panel on 36 characters.
func shortRef(ref string) string {
	if i := strings.IndexByte(ref, '-'); i > 0 {
		return ref[:i]
	}
	if len(ref) > 8 {
		return ref[:8]
	}
	return ref
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// digWaitFor resolves the excavation an order is waiting behind, for the
// sentence. Returns the dig's order id; zero when there is no dig on that lane or
// the read fails.
//
// It used to return the target slot as well, resolved through digTargetOf. That
// second value has been "" everywhere since the folder died — see
// QueueParams.DigOrderID for the whole of why it is gone rather than repaired.
//
// ── IT ASKS THE LOCK, WHICH IS THE ONE PLACE THAT KNOWS ───────────────────
//
// The lane lock's rows ARE the record of which dig holds which lane. Deriving it
// any other way — a child walk, a status scan — is the archaeology
// unlockLaneForCompound had to unlearn, and it was wrong three separate ways
// there.
//
// ── AND IT ASKS FOR THE EXCAVATION, NOT THE HOLDER (§R.101) ───────────────
//
// This paragraph used to end "and DigOwner is its one spelling of that
// question". DigOwner answers who holds the lane EXCLUSIVELY, and since §R.101
// gave every demand's source hold mode='dig' that is no longer the same set as
// "who is excavating it". The clause this feeds renders the words "dig %d is
// working this lane", so a plain retrieve sourcing from the lane was being
// announced to the floor as an excavation with an id attached — a false sentence
// that is worse than the missing clause it replaced, because it can be acted on.
//
// ExcavationOwner is the read that matches the words. When the lane is held by a
// source lock this now answers 0 and the caller renders the sentence without the
// clause — the same disposition as an unreadable row, and for the same reason:
// an operator is better served by the lane alone than by an id that names the
// wrong thing.
//
// A READ FAILURE ANSWERS "NOT KNOWN", not "no dig". The caller renders the
// sentence without the clause, which is exactly what it rendered before this
// existed. A wait sentence is the wrong place to fail: the order is parked
// either way, and an operator is better served by the lane alone than by a dig
// id that might name the wrong excavation.
// It is a FREE FUNCTION, not a method, because two types ask it: the Dispatcher
// (the complex-reshuffle park arms) and the PlanningService (the plain path's
// right-of-way refusal). Both hold the same two fields; a method on either would
// have produced a second spelling on the other.
func digWaitFor(laneLock *LaneLock, laneID int64) int64 {
	if laneLock == nil || laneID == 0 {
		return 0
	}
	digID, err := laneLock.ExcavationOwner(laneID)
	if err != nil || digID == 0 {
		return 0
	}
	return digID
}

// digWaitByLaneName is digWaitFor for the call sites that hold a lane NAME
// rather than an id — the right-of-way refusal names the lane it was refused by,
// not the one it was planning against.
func digWaitByLaneName(db *store.DB, laneLock *LaneLock, laneName string) int64 {
	if laneName == "" {
		return 0
	}
	lane, err := db.GetNodeByDotName(laneName)
	if err != nil || lane == nil {
		return 0
	}
	return digWaitFor(laneLock, lane.ID)
}

// digWaitForEpisode names the excavation already serving this demand's episode.
//
// The one-dig-per-episode arm refuses because the plant is ALREADY digging for
// this demand — on a different lane than the one just refused, which is why the
// lane in that sentence cannot answer "what am I waiting for". The dig id comes
// off the result structurally (laneClearResult.blockingDig), not out of the
// error text.
func digWaitForEpisode(res laneClearResult) int64 {
	return res.blockingDig
}
