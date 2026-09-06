// carrier.go — the vocabulary for "what is standing on this node" as distinct
// from "what was asked for".
//
// ── WHY THIS EXISTS ───────────────────────────────────────────────────────
//
// The Edge holds two different facts that both spell themselves as a payload
// code, and nothing in the type system told them apart:
//
//   - the REQUESTED identity — what a supply request asked for, which is a
//     property of the style_node_claims row the process's active (or target)
//     style has for this node; and
//   - the LINESIDE identity — what carrier is physically on the node right
//     now, which is a property of the bin and is a fact only Core holds.
//
// Reading the first where the second was meant is the defect class behind
// SMN_029 (Springfield, 2026-09-02): a changeover moved a cell onto a new
// style while the old style's carrier was still standing on it, and every
// path that asked "whose bin is this" got the incoming style's answer about
// the outgoing style's bin.
//
// ONE NAMED TYPE, NOT TWO. This said "the compiler now refuses that
// assignment. LinesidePayloadCode and RequestedPayloadCode are distinct
// types" — and RequestedPayloadCode had zero non-test callers, so there was
// no assignment anywhere for the compiler to refuse, and the runtime row held
// the lineside code as a plain string that any requested one could be assigned
// to. The pair has been cut to the half that carries the guarantee: the
// REQUESTED identity stays a plain string, because it is what every claim and
// every wire field already is, and the LINESIDE identity is named. A claim's
// payload therefore cannot be assigned to the runtime row's lineside field
// without a written-down conversion.
//
// SCOPED TO NEW SURFACES, exactly as domain/loader.go's typed identifiers are:
// this is not a repo-wide rename of PayloadCode. Legacy call sites convert at
// the boundary, and a conversion is a place where somebody decided which
// question they were answering.

package domain

// LinesidePayloadCode is the payload of the carrier PHYSICALLY on a node.
// Only Core can answer it: this side stores active_bin_id, an opaque Core id,
// and has no bins table.
//
// The requested identity has no counterpart type on purpose. It is a plain
// string everywhere already — on claims, on the wire, in the payload catalog —
// and naming it would mean converting at hundreds of sites for a distinction
// only one side of needs enforcing. Naming the lineside one is enough: it is
// the field a requested value must not be assigned to.
type LinesidePayloadCode string

func (p LinesidePayloadCode) String() string { return string(p) }

// LinesideCarrier is what is standing on a node, and whether that could be
// established at all.
//
// TWO STATES, AND THEY ARE NOT INTERCHANGEABLE. A known carrier with an empty
// payload is an EMPTY carrier — a real, common, actionable answer. An unknown
// carrier is "nobody could tell me", and a caller must not act on it.
// Collapsing the two is how an empty bin and an unreadable node came to look
// alike, which is the same mistake Core's carrierPayloadFor (dispatch/
// loader_place.go) and the node-bins tri-state were each written to avoid.
//
// THE ZERO VALUE IS UNUSABLE ON PURPOSE: it is the unknown carrier, so a
// LinesideCarrier that nobody filled in cannot be mistaken for an answer.
// Payload() refuses to hand out a code without the caller seeing ok=false.
//
// Deliberately only these two states. A provenance enum naming every way the
// answer was obtained would have no reader that branches on it today, and an
// enum nobody switches on is a comment with a type. Add a case when a caller
// needs to tell two sources apart.
type LinesideCarrier struct {
	payload LinesidePayloadCode
	known   bool
}

// KnownCarrier records that the node's carrier was established. Pass "" for an
// empty carrier — that is an answer, not an absence.
func KnownCarrier(payload LinesidePayloadCode) LinesideCarrier {
	return LinesideCarrier{payload: payload, known: true}
}

// UnknownCarrier records that the question could not be answered. Equal to the
// zero value, so a struct nobody filled in reads as "cannot say".
func UnknownCarrier() LinesideCarrier { return LinesideCarrier{} }

// Payload returns the lineside payload and whether it is an answer at all.
// The bool is not a nil-check: false means unreadable, and "" with true means
// the carrier is empty.
func (c LinesideCarrier) Payload() (LinesidePayloadCode, bool) {
	if !c.known {
		return "", false
	}
	return c.payload, true
}

// Known reports whether the carrier could be established. For callers that
// only need to decide whether to act, not what to act on.
func (c LinesideCarrier) Known() bool { return c.known }

// IsEmpty reports a carrier that is known to be carrying nothing. False for an
// unknown carrier — "I cannot tell" is not "it is empty", and treating it as
// such is how an unreadable node gets material ordered onto it.
func (c LinesideCarrier) IsEmpty() bool { return c.known && c.payload == "" }

// CarrierSource names who is asserting what the carrier at a node is.
//
// EVERY WRITE OF THE LINESIDE IDENTITY GOES THROUGH ONE DOORWAY AND NAMES ONE
// OF THESE. The point is not to rank them — an automatic source is not more
// trustworthy than a person here — it is that a later reader can tell which
// question was answered. The failure this replaces is a field written by eight
// paths, most of them stamping the requested style, with nothing recording
// which had last spoken.
//
// This mirrors the demand vocabulary, where demand arrives by threshold or by
// hand and demand-by-hand is never swept or silently re-aimed.
type CarrierSource string

const (
	// CarrierFromDelivery: Core named the carrier on the delivery envelope,
	// read off the bin row itself. The ordinary case.
	CarrierFromDelivery CarrierSource = "delivery"

	// CarrierFromOperator: a person put this carrier here and said what it is.
	// ON A MANUAL LOAD THE OPERATOR IS THE FINEST INSTRUMENT IN THE ROOM —
	// no envelope arrives ahead of them, so before this there was simply no
	// answer. Their assertion is first-class, not a fallback.
	CarrierFromOperator CarrierSource = "operator"

	// CarrierDeparted: the carrier left. Its identity leaves with it, or the
	// next occupant inherits it and it is read as a fact about them.
	CarrierDeparted CarrierSource = "departed"
)

// NOTE ON CYCLE COUNTING, deliberately absent from the list above.
//
// A count does not assert identity and must not be routed here. Core's
// bin-count response carries the bin id, the expected and actual counts and
// the epoch — and no payload. The operator counted PARTS, not what the carrier
// is, and turning a count into an identity claim would invent an answer nobody
// gave. This matters because the count path is exactly the one that used to
// re-stamp the requested claim over the carrier standing there and disarm the
// evacuation fix for that carrier's whole stay.
