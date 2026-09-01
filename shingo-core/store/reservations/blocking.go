package reservations

import "fmt"

// ── THE BLOCKING QUESTION HAS ONE SPELLING, AND IT IS HERE ──────────────────
//
// "Does a reservation row on this resource make it unavailable?"
//
// That question was written out by hand at twenty-three queries in eight
// packages. Nothing was wrong with any single copy — they agree today, and a
// drift test now keeps them agreeing — but twenty-three copies of a predicate
// is twenty-three edits the day the answer changes, and the answer is about to
// change: a reservation is Core sourcing ahead of the call, and a reservation
// must not hide a bin from the demand that is being called for. When that lands
// it is one string in this file, not a sweep somebody has to get right
// twenty-three times in a row.
//
// TWO STATE SPELLINGS, AND THEY ARE NOT THE SAME QUESTION. The find side asks
// `state = 'pending'` and the book-keeping side asks
// `state IN ('pending','confirmed')`, and the difference is deliberate:
//
//   - The FIND side is looking for a bin to give somebody. A confirmed row
//     coincides with a hard `bins.claimed_by` — the one-tx claim+confirm moves
//     them together — and every find query already excludes claimed bins, so
//     asking for 'confirmed' as well would be asking the same thing twice.
//     See BinJoinQuery's own note on the projector column.
//   - The BOOK-KEEPING side (the steal's victim scan, the reconciliation
//     sweeps, the burial instrument, the lane cordons) is asking whether a row
//     EXISTS at all, in either state. That is the same predicate the partial
//     unique indexes carry (`uq_reservations_bin_active`, migrations v43/v44),
//     and it is also, since v44, every row on disk — release is a hard delete,
//     so there is no third state to leave behind.
//
// They are kept as two named forms rather than unified, because unifying them
// would be a behaviour change wearing a refactor's clothes.
//
// WHAT DOES NOT LIVE HERE. The dig-lock question (who a cordon on a lane mouth
// keeps out) is dig_exclusion.go's, and the occupancy rows are a presence
// witness — a robot physically inside a corridor — which is not an ownership
// claim and must never be folded into a blocking predicate. Both compose
// ActiveStateSQL for the state half and nothing else from this file.
//
// TestBlockingPredicateHasExactlyOneSpelling is what keeps the count at one.

// The two state predicates, each spelled once. Unexported: a caller wanting the
// state half alone goes through BlockingStateSQL / ActiveStateSQL, which is what
// makes the qualifier explicit at the call site.
const (
	blockingState = `state = 'pending'`
	activeStates  = `state IN ('pending','confirmed')`
)

// BlockingStateSQL renders "this row makes its resource unavailable", qualified
// for the caller's alias — pass "r." for a subquery aliased r, or "" when the
// column is unambiguous.
func BlockingStateSQL(qualifier string) string { return qualifier + blockingState }

// ActiveStateSQL renders "this row is on the books at all", qualified the same
// way. This is the index predicate, and since v44's CHECK it matches every row.
func ActiveStateSQL(qualifier string) string { return qualifier + activeStates }

// BinSpokenForSQL is the whole EXISTS the find side asks: is this bin already
// promised to somebody?
//
// IT NAMES ITS ALIASES, WHICH IS WHY IT IS A CONST AND NOT A RENDERER. Every
// bin-reading query in this codebase aliases bins as `b` — BinJoinQuery and
// BinFromClause make that the house contract, and the nine readers of this
// predicate all obeyed it before this fragment existed. Fixing `b` here buys a
// constant expression, which is what lets BinJoinQuery and EmptyCarrierWhere —
// both `const` — compose it instead of being rebuilt per call.
//
// Callers negate it themselves (`AND NOT ` + BinSpokenForSQL), because the
// positive form is what the projector column below needs.
const BinSpokenForSQL = `EXISTS (SELECT 1 FROM reservations r WHERE r.bin_id = b.id AND r.` + blockingState + `)`

// HeldByOwnerSQL is the CLAIM SEATBELT's half: does THIS order hold a live
// reservation on this resource, so that its hard claim is allowed to land?
//
// The opposite direction of travel from BinSpokenForSQL — owner-keyed and
// positive — and it is the reason "one blocking predicate" is not literally one
// string. A claim that fires without a reservation behind it is the split-brain
// this clause closed; it is not an availability question and must not move when
// availability does.
//
// orderParam and resourceParam are 1-based positional parameter numbers.
//
// THE BIN FORM CARRIES NO resource_kind, DELIBERATELY: bin_id is non-NULL on
// bin rows and NULL on every other kind (the exactly-one-of CHECK, v44), so
// bin_id alone already scopes the match. The slot form needs the kind because
// node_id is shared by slot, mouth and occupancy rows.
func HeldByOwnerSQL(kind Kind, orderParam, resourceParam int) string {
	if kind == KindBin {
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM reservations WHERE order_id=$%d AND bin_id=$%d AND %s)`,
			orderParam, resourceParam, blockingState)
	}
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM reservations WHERE order_id=$%d AND node_id=$%d AND resource_kind='%s' AND %s)`,
		orderParam, resourceParam, string(kind), blockingState)
}

// SlotSpokenForByStrangerSQL is the destination-side question: is this slot on
// somebody ELSE's book?
//
// Owner-aware where the bin form is owner-blind, and both states where the bin
// form is pending-only — both differences are load-bearing. A store leg must be
// allowed to pick the slot it has already reserved for itself (owner-aware), and
// a slot whose reservation is confirmed is a slot a robot is driving to, which is
// exactly what a stranger must not seal (both states).
//
// alias names the subquery's reservations alias; nodeExpr is the candidate slot's
// id expression; ownerExpr is the asking order's id — a bind placeholder like
// "$2", or a format-rendered one from the caller's own Sprintf.
func SlotSpokenForByStrangerSQL(alias, nodeExpr, ownerExpr string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM reservations %[1]s
			 WHERE %[1]s.node_id = %[2]s
			   AND %[1]s.resource_kind = '%[4]s'
			   AND %[1]s.%[5]s
			   AND %[1]s.order_id <> %[3]s)`,
		alias, nodeExpr, ownerExpr, string(KindSlot), activeStates)
}

// OnTheBooksSQL asks whether a resource carries ANY active reservation row,
// owned by anybody. The reconciliation sweeps' half of the two-books comparison:
// a hard claim with no row behind it is drift.
//
// RESOURCE-KEYED AND OWNER-BLIND, WHICH IS CORRECT ONLY WHILE ONE ROW PER
// RESOURCE IS STRUCTURAL. uq_reservations_{bin,slot}_active is what makes "a row
// exists" and "the claim-holder's row exists" the same sentence. The moment that
// index is narrowed, this predicate must gain the owner or a stranger's row
// starts shielding the very drift the sweep exists to end — a named function is
// so that change is one edit with its readers listed.
//
// resourceExpr is the bin's or the slot's id expression in the outer query.
func OnTheBooksSQL(kind Kind, resourceExpr string) string {
	col := "bin_id"
	if kind != KindBin {
		col = "node_id"
	}
	return fmt.Sprintf(`EXISTS (
		      SELECT 1 FROM reservations r
		      WHERE r.resource_kind = '%s' AND r.%s = %s
		        AND r.%s
		    )`, string(kind), col, resourceExpr, activeStates)
}
