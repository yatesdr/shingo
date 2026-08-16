package reservations

import "fmt"

// ── THE DIG-LOCK QUESTION HAS ONE SPELLING, AND IT IS HERE ──────────────────
//
// "Does the dig holding this lane exclude the order that is asking?"
//
// That question had three answers, and they disagreed:
//
//   - ADMISSION (dispatch.ownsDig) exempted the asker and its compound parent.
//     Correct: in expose mode the lane lock is TRANSFERRED to the complex
//     parent, and that parent's own pickup is what releases it. Refusing the
//     owner would not be a wait, it would be a wedge.
//   - SOURCING (store.ListChildNodesUnlocked) exempted nobody. Its filter was a
//     consolidation of five copies of `if LaneLock.IsLocked(child) { continue }`
//     — a real improvement that consolidated onto the WRONG semantics, because
//     IsLocked has no asker to exempt.
//   - DIG PLANNING (dispatch.findShuffleSlots) did not ask at all.
//
// The cost of the disagreement, observed on the lane-stress rig 2026-08-10 and
// the reason this file exists: an expose dig completes, the lock transfers to
// the complex parent to protect the bin it just uncovered, the parent resumes
// and re-resolves — and sourcing drops the lane BECAUSE OF THE PARENT'S OWN
// LOCK. The parent cannot see the bin its own dig exposed for it. It resolves
// to the next-oldest buried bin instead and plans another dig, and the ring
// arrests. The lock that protects the bin hid it from the only order allowed
// to take it.
//
// One fact, three readers, three semantics. Both halves below exist so that
// cannot recur: ExcludedBy answers it in Go, DigExclusionSQL answers it in
// SQL, and the SQL is RENDERED rather than written, so a query cannot spell it
// differently without deleting the renderer. TestDigExclusionHasExactlyOneSQLSpelling
// fails if a second spelling appears; TestDigExclusion_AllThreeReadersAgree
// fails if the readers ever disagree about a concrete case.
//
// This mirrors what store/internal/helpers/lane_reachability.go already did for
// the reachability predicate, and for the same reason: the copies of a shared
// question drift in ways no single call site can see.

// DigAsker is the order a dig-lock question is being asked ON BEHALF OF.
//
// Two ids because a dig lock is held by ONE order but legitimately serves TWO:
// a compound child works inside a lane its compound parent locked, and in
// expose mode the parent inherits a lock its children took. laneOwnerFor is
// the dispatch-side resolution of that pair; this type is what it resolves to.
type DigAsker struct {
	// OrderID is the order that wants the lane.
	OrderID int64
	// LaneOwner is the order that would hold a lane lock on OrderID's behalf:
	// its compound parent, or OrderID itself when it has none.
	LaneOwner int64
}

// Anyone is the asker for a question with no particular order behind it.
//
// It is excluded by every dig — order ids are never zero, so both comparisons
// in ExcludedBy fail open to "excluded". That is deliberate and it is what
// makes adopting this predicate safe: a call site that has no order in hand
// keeps exactly the owner-blind behaviour it had before, and only the sites
// that thread a real asker change.
var Anyone = DigAsker{}

// AskerFor builds the asker for an order and the order that owns lane locks on
// its behalf. Pass laneOwner == orderID when the order has no compound parent.
func AskerFor(orderID, laneOwner int64) DigAsker {
	if laneOwner == 0 {
		laneOwner = orderID
	}
	return DigAsker{OrderID: orderID, LaneOwner: laneOwner}
}

// ExcludedBy reports whether a dig held by digOwner shuts this asker out.
//
// digOwner == 0 means no dig holds the lane, which excludes nobody. Note the
// direction: this answers "keep out", so the OWNER arms return false. Callers
// that want "may I" invert it.
func (a DigAsker) ExcludedBy(digOwner int64) bool {
	if digOwner == 0 {
		return false
	}
	return digOwner != a.OrderID && digOwner != a.LaneOwner
}

// Args returns the two bind values DigExclusionSQL's placeholders take, in the
// order the placeholders were named. Kept beside the renderer so a caller
// cannot pass them in the wrong order or forget one.
func (a DigAsker) Args() []any { return []any{a.OrderID, a.LaneOwner} }

// DigExclusionSQL renders ExcludedBy as a SQL predicate over ownerCol.
//
// ownerCol is the qualified owner column of the dig reservation row (e.g.
// "dig_hold.order_id"); askerParam and laneOwnerParam are the 1-based
// positional parameter numbers that will carry DigAsker.Args().
//
// The fragment is a literal transcription of ExcludedBy's two comparisons.
// It is rendered rather than written out at the query so that the two cannot
// drift: there is no second place where the comparison is spelled.
func DigExclusionSQL(ownerCol string, askerParam, laneOwnerParam int) string {
	return fmt.Sprintf("%s <> $%d AND %s <> $%d",
		ownerCol, askerParam, ownerCol, laneOwnerParam)
}
