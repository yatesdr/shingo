package dispatch

import (
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// SourceIntent is the Stage-4 data home for the sourcing reads that used to
// branch on OrderType — retrieve_empty's empty-carrier intent, move's node-local
// sourcing, and the empty-payload guard's exemptions. It is set ONCE at intake
// (the label→data carve-out, via SourceIntentForType) and read downstream as
// data by the source finder and the scanner. Stored as a plain string column.
const (
	SourceIntentFull  = ""      // retrieve: a payload-matched FULL bin (the default)
	SourceIntentEmpty = "empty" // retrieve_empty: an empty compatible carrier
	SourceIntentLocal = "local" // move: the bin AT a concrete source node (node-local)
)

// SourceIntentForType maps an order's type to its sourcing intent. It is called
// only at intake, where reading the type to stamp the data field is the legitimate
// label→data conversion (Stage-5 forbidigo carve-out) — every downstream reader
// keys on order.SourceIntent, never the type.
func SourceIntentForType(t protocol.OrderType) string {
	switch t {
	case OrderTypeRetrieveEmpty:
		return SourceIntentEmpty
	case OrderTypeMove:
		return SourceIntentLocal
	default:
		// Retrieve (full) falls here — a payload-matched full bin via the finder.
		return SourceIntentFull
	}
}

// IsCoordinated is the Stage-3 dispatch discriminator: it reports whether an
// order carries an Edge-authored coordinated (multi-leg) plan, i.e. whether it
// is a complex/changeover/swap order rather than a plain single-transport one.
// It REPLACES the OrderType read that used to select the collision gate and
// dispatch tail — dispatch control flow branches on this plan-provenance signal,
// not on the type label.
//
// The signal is now the order.Coordinated COLUMN, stamped once at intake (complex
// intake → true, every other intake → false; backfilled from steps_json). It used
// to be StepsJSON != "", but that heuristic is unsound the moment a simple plan
// persists to steps_json — a plain order and a coordinated changeover leg can
// be byte-identical plans ([pickup@line, dropoff@storage]), so no structural
// predicate separates them; only provenance does. The column IS that provenance.
//
// The organizing principle this discriminator expresses: a simple order is
// add-only — it owns exactly one bin and has no incumbent-moving (evac) leg — so
// it WAITS at both ends (source empty → wait, dest occupied → wait). An order is
// coordinated exactly when resolving a conflict needs a second leg to move another
// bin. IsCoordinated is really hasEvacLeg.
func IsCoordinated(order *orders.Order) bool {
	return order.Coordinated
}

// AssertSimpleNotCoordinated is the tripwire protecting the dispatch
// discriminator: a plain-class order (a simple single-transport type) must never
// be classified coordinated, or the discriminator inverts and routes it to the
// coordinated tail (role gate + complex reserve/confirm — the round-7 leak). It
// fails loudly.
//
// It keys on order.Coordinated, NOT StepsJSON: steps-presence is not a proxy for
// coordinated (a plain order may carry a plan; the column is the provenance), so a
// StepsJSON check would misfire. The name says "NotCoordinated" because that is the
// invariant — the old "HasNoSteps" name lied, since the body has always checked the
// column. The OrderType read here is a legitimate ASSERTION, not control flow (a
// Stage-5 forbidigo carve-out) — a plain-family label stamped coordinated is a
// construction bug we want surfaced.
func AssertSimpleNotCoordinated(order *orders.Order) {
	switch order.OrderType {
	case OrderTypeRetrieve, OrderTypeRetrieveEmpty, OrderTypeMove:
		if order.Coordinated {
			log.Printf("CONSTRUCTION BUG: plain-family order %d (%s) is stamped coordinated — "+
				"the dispatch discriminator (IsCoordinated) will route it to the coordinated tail "+
				"(role gate + complex reserve/confirm). Fix the intake stamp.", order.ID, order.OrderType)
		}
	}
}
