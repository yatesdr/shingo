package dispatch

import (
	"log"

	"github.com/google/uuid"

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
	// SourceIntentOnDeck is the carried-bin recovery: the bin is already on the
	// pinned robot's deck, so there is nothing to source and nothing to pick
	// up. It is the one intent that changes the PLAN rather than the search —
	// the order is a single unload — and the one that pins a vehicle.
	SourceIntentOnDeck = "on_deck"
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

// classifyInboundOrigin is the intake half of the demand grain: it turns what an
// Edge said about an order's origin into the (origin_id, origin_class) pair Core
// stores. Called at the TWO intake sites — CreateInboundOrder and
// HandleComplexOrderRequest — and nowhere else. (There used to be a third: the
// buried branch built its own parent. It now falls through to the same create,
// so there are two intake sites, not three.)
// Derivative orders do not come through here; they inherit the parent's pair
// verbatim, because re-classifying a child would let a parent's judgement be
// overturned one level down.
//
// The three outcomes, and why an absent origin is NOT automatically a finding:
//
//   - an id, and it parses          → attached. The id wins over whatever class
//     came with it; an order carrying an episode IS attached by definition.
//   - no id, class says no_demand   → no_demand. Edge stamped this at ITS create
//     site, where the answer was known. Opportunistic loader staging and the
//     unloader U1 full-in are the two that reach here.
//   - anything else                 → orphan. Should have had an episode and
//     didn't.
//
// SKEW IS THE MOTIVATING CASE AND IT LANDS ORPHAN ON PURPOSE. Edge ships before
// Core, so during the window a plant runs new Core against an old Edge every
// threshold-driven order arrives with no origin at all — indistinguishable, at
// this seam, from a genuine lost origin. Guessing no_demand to keep the bucket
// quiet would hide the real ones for as long as the skew lasted; orphan is the
// honest reading, and the Core sweep's childless auto-close is what keeps the
// deploy artifact from reading as a permanent alarm.
//
// A MALFORMED ID DOES NOT FAIL THE ORDER. origin_id is a UUID column, so a
// non-UUID string would abort the INSERT and lose the transport work over a
// telemetry field. It lands orphan with a loud log instead — the same
// accept-and-log posture HandleDemandOrigin takes on an unparseable episode key,
// and for the same reason: the order is worth more than its attribution, and
// dropping the evidence is what makes such a bug unfindable.
func classifyInboundOrigin(originID, originClass, station, orderUUID string) (string, string) {
	if originID != "" {
		if _, err := uuid.Parse(originID); err != nil {
			log.Printf("dispatch: MALFORMED origin_id %q on order %s from station=%s: %v — "+
				"order created as an ORPHAN rather than rejected; some mint site is not using uuid",
				originID, orderUUID, station, err)
			return "", protocol.OriginClassOrphan
		}
		return originID, protocol.OriginClassAttached
	}
	switch originClass {
	case protocol.OriginClassNoDemand:
		return "", protocol.OriginClassNoDemand
	case "", protocol.OriginClassOrphan, protocol.OriginClassAttached:
		// "" is the old-Edge / unstated case. `attached` with no id is a
		// contradiction the sender got wrong; it has nothing to attach TO, so
		// it is exactly what orphan describes.
		return "", protocol.OriginClassOrphan
	default:
		log.Printf("dispatch: UNKNOWN origin_class %q on order %s from station=%s — "+
			"order created as an ORPHAN; origin_class is a closed enum (see protocol.OriginClass*)",
			originClass, orderUUID, station)
		return "", protocol.OriginClassOrphan
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
