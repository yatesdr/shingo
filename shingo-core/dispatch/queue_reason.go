package dispatch

import (
	"fmt"
	"strings"

	"shingo/protocol"
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
//   - QueueStorageRearranging: Lane + Payload (+ Step)
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
// Wording is owner-pinned; a snapshot test pins the exact strings.
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
func rearrangingSentence(p QueueParams) string {
	switch {
	case p.Lane != "" && p.Payload != "":
		return fmt.Sprintf("Rearranging lane %s to reach %s", p.Lane, p.Payload)
	case p.Lane != "":
		return fmt.Sprintf("Rearranging lane %s to reach this material", p.Lane)
	case p.Payload != "":
		return fmt.Sprintf("Rearranging storage to reach %s", p.Payload)
	default:
		return "Rearranging storage to reach this material"
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
