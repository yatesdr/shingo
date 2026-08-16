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
//   - QueueStorageRearranging: Lane + Payload + DigOrderID/DigTarget (+ Step)
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
	// DigOrderID and DigTarget name the EXCAVATION this order is waiting on: the
	// dig's order id, and the slot holding the bin it is uncovering.
	//
	// ── WHY A WAIT NAMES THE DIG AND NOT JUST THE LANE ────────────────────
	//
	// "Rearranging lane LSD_01 to reach PANEL-B" is true and it is one word plus
	// a lookup. An operator watching a demand sit there has to go and find which
	// dig is running, what it is digging for, and whether it is the one that will
	// release them — three questions, on a different page, against a lane that may
	// have had several digs on it during the wait.
	//
	// The dig id is the join key for all three. Naming it, with the target slot,
	// turns the wait into a sentence that can be acted on without leaving the
	// board: THIS order is waiting for THAT dig to uncover THAT slot.
	//
	// Both are optional. A wait that genuinely has no identifiable dig — a lane
	// held by an ordinary order, a plan refused before any dig was proposed —
	// renders exactly as it did before, because a dig id we could not resolve
	// must not be invented. Zero and empty are "not known", not "none".
	DigOrderID int64
	DigTarget  string
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
	return s + digClause(p)
}

// digClause appends the excavation's identity to a rearranging sentence.
//
// Three shapes, because the two facts arrive independently: a dig resolved from
// a lane lock always has an id and may have no target (a dig clearing a slot to
// drop into uncovers nothing), while a target with no id means the resolution
// failed and inventing one would be worse than saying less.
//
// Empty when neither is known, so an ordinary lane-locked wait reads exactly as
// it did before this existed.
func digClause(p QueueParams) string {
	switch {
	case p.DigOrderID != 0 && p.DigTarget != "":
		return fmt.Sprintf(" — dig %d is uncovering %s", p.DigOrderID, p.DigTarget)
	case p.DigOrderID != 0:
		return fmt.Sprintf(" — dig %d is working this lane", p.DigOrderID)
	case p.DigTarget != "":
		return fmt.Sprintf(" — uncovering %s", p.DigTarget)
	default:
		return ""
	}
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
// sentence. Returns the dig's order id and the slot it is uncovering; zero and
// empty when there is no dig on that lane or the read fails.
//
// ── IT ASKS THE LOCK, WHICH IS THE ONE PLACE THAT KNOWS ───────────────────
//
// The lane lock's rows ARE the record of which dig holds which lane, and
// DigOwner is its one spelling of that question. Deriving it any other way — a
// child walk, a status scan — is the archaeology unlockLaneForCompound had to
// unlearn, and it was wrong three separate ways there.
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
func digWaitFor(db *store.DB, laneLock *LaneLock, laneID int64) (int64, string) {
	if laneLock == nil || laneID == 0 {
		return 0, ""
	}
	digID, err := laneLock.DigOwner(laneID)
	if err != nil || digID == 0 {
		return 0, ""
	}
	return digID, digTargetOf(db, digID)
}

// digTargetOf reads the slot a dig is uncovering. Empty when the order cannot be
// read, or when it is a dig that uncovers no bin (one clearing a slot to drop
// into) — both of which render as the id alone.
func digTargetOf(db *store.DB, digID int64) string {
	if digID == 0 {
		return ""
	}
	dig, err := db.GetOrder(digID)
	if err != nil || dig == nil {
		return ""
	}
	return dig.DigTargetNode
}

// digWaitByLaneName is digWaitFor for the call sites that hold a lane NAME
// rather than an id — the right-of-way refusal names the lane it was refused by,
// not the one it was planning against.
func digWaitByLaneName(db *store.DB, laneLock *LaneLock, laneName string) (int64, string) {
	if laneName == "" {
		return 0, ""
	}
	lane, err := db.GetNodeByDotName(laneName)
	if err != nil || lane == nil {
		return 0, ""
	}
	return digWaitFor(db, laneLock, lane.ID)
}

// digWaitForEpisode names the excavation already serving this demand's episode.
//
// The one-dig-per-episode arm refuses because the plant is ALREADY digging for
// this demand — on a different lane than the one just refused, which is why the
// lane in that sentence cannot answer "what am I waiting for". The dig id comes
// off the result structurally (serviceDigResult.blockingDig), not out of the
// error text.
func digWaitForEpisode(db *store.DB, res serviceDigResult) (int64, string) {
	if res.blockingDig == 0 {
		return 0, ""
	}
	return res.blockingDig, digTargetOf(db, res.blockingDig)
}
