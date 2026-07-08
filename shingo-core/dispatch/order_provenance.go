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
		// Retrieve (full) and store both fall here. Store self-sources in
		// planStore (claims a bin AT its node by availability, never via the
		// finder), so it has no finder sourcing-intent — the default "" is the
		// honest classification AND keeps this re-homing behavior-neutral: the
		// old payload guard fired for a blank-payload store (OrderType != Move),
		// so store must stay non-exempt, i.e. NOT SourceIntentLocal.
		return SourceIntentFull
	}
}

// IsCoordinated is the Stage-3 dispatch discriminator: it reports whether an
// order carries an Edge-authored coordinated (multi-leg) plan, i.e. whether it
// is a complex/changeover/swap order rather than a plain single-transport one.
// It REPLACES the OrderType read that used to select the collision gate and
// dispatch tail — dispatch control flow now branches on this plan-provenance
// signal, not on the type label.
//
// The signal is StepsJSON presence. A plain single-transport order and a
// coordinated changeover leg can be byte-identical plans (same nodes, no wait —
// e.g. a store and BuildReleaseSteps are both [pickup@line, dropoff@storage]),
// so NO structural predicate separates them; the difference is provenance. Today
// StepsJSON != "" ⟺ OrderType == Complex exactly: complex intake always
// populates it (and rejects 0-step orders), simple intake never does.
//
// FRAGILITY: this breaks if a simple (single-transport) plan is ever persisted
// to order.StepsJSON. Simple plans are emitted transiently at intake (order-
// builder Stages 1-2, on PlanningResult.Plan) and are NEVER persisted — the
// plain dispatch tail runs off order fields + the shared SourceFinder, it does
// not unmarshal a plan. AssertSimpleHasNoSteps is the tripwire guarding that
// precondition.
func IsCoordinated(order *orders.Order) bool {
	return order.StepsJSON != ""
}

// AssertSimpleHasNoSteps is the tripwire protecting IsCoordinated's precondition:
// a simple-family order must never carry a persisted step plan, or the
// discriminator silently inverts (a plain order misclassified as coordinated
// would be routed to the role gate + fast-path — the round-7 leak). It fails
// loudly rather than let that happen.
//
// The OrderType read here is a legitimate ASSERTION, not control flow — it is a
// carve-out for the Stage-5 forbidigo lint (a plain-family label with a
// coordinated plan is a construction bug we want surfaced, exactly the kind of
// invariant an assertion exists to catch).
func AssertSimpleHasNoSteps(order *orders.Order) {
	switch order.OrderType {
	case OrderTypeRetrieve, OrderTypeRetrieveEmpty, OrderTypeMove, OrderTypeStore:
		if order.StepsJSON != "" {
			log.Printf("CONSTRUCTION BUG: simple-family order %d (%s) has StepsJSON populated — "+
				"the dispatch discriminator (IsCoordinated) will misclassify it as coordinated and "+
				"route it to the role gate + line fast-path. Fix the intake path.", order.ID, order.OrderType)
		}
	}
}
