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
//   - EMPTY SELECTION (store/bins's four empty finders) did not ask at all
//     either, and was the unlisted FOURTH reader. Added in MG3-1b, through
//     NotForeignDugArm, which renders the predicate below rather than spelling
//     it — so the count went from three answers to four readers of ONE.
//   - THE BIN CLAIM (reservations.acquire) is the FIFTH, and the only one that is
//     not a filter. The four above all answer at FIND time, and a find-time answer
//     cannot hold: measured 2026-08-31, order 43 resolved a lane at :27.505 while
//     it carried no dig row, order 42 took MOUTH/dig on it at :27.583, and 43 was
//     granted a bin inside that lane at :27.610. 43 then owned the one bin 42's
//     dig had to move, in a corridor 43 could never enter — two robots stood 122
//     sim-minutes and the payload behind 42 went dark. The acquire is the
//     serialization point, so it asks there, against the insert's own snapshot.
//
// The empty finders are the reason this list is worth keeping accurate. This
// file declared the dig-lock question closed at exactly three readers; a closed
// accounting is the worst possible place to carry an unlisted fourth, because
// the closure is what stops anyone looking.
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

// StagedOutside is the set of orders whose robots are parked at one of a node
// GROUP's wait points rather than inside any of its corridors — gate-staged, in
// the dispatch layer's vocabulary.
//
// ── A WAIT POINT IS GROUP STAGING, NOT A LANE'S DOORWAY (owner, 2026-08-31) ─
//
// This is the fact the whole predicate rests on and it is easy to get backwards,
// because the marks have lane-shaped NAMES. "Lane_01-WAIT" is a map point that
// happens to be painted near lane 1. It is not lane 1's doorway. The wait points
// of a group are a shared staging area in front of ALL its lanes, and a robot
// standing at any of them has not entered a corridor and has not been committed
// to one — the oracle can still send it to lane 10.
//
// So membership is scoped to the GROUP: an order staged at any of a group's wait
// points is standing outside every lane in that group, and obstructs an
// excavation in none of them. Scoping it to "the lane whose name the mark
// carries" would be reading the paint rather than the geometry.
//
// ── WHAT IT IS FOR: THE ARM PROTECTS THE CORRIDOR ─────────────────────────
//
// A robot waiting at a mark holds its lane's inbound mouth row, and that row is
// doing honest work: it is what tells the release pass "this one is still
// coming", so a shallower store cannot wall it in. What it must NOT do is turn
// away a robot that needs to WORK the corridor, because the robot holding it is
// standing outside that corridor. The two do not physically contend.
//
// THE ARM DOES NOT CARE WHICH KIND OF DIG-MODE ROW IS ASKING, and getting that
// wrong cost a whole run. What matters is that the requester needs the corridor
// and the holder is standing outside it. An excavation compound and a §R.101
// source-lock retrieve are the same case here: both are ModeDig, both are a
// robot about to drive into the lane, and a robot at the mark obstructs neither.
//
// Both halves were measured, one run apart, on the same lane:
//
//	2026-08-30, gated sim. Three EXCAVATIONS raised for orders 23, 62 and 76
//	were refused on Lane_08's mouth, held by order 22 — gate-staged at that
//	lane's mark, waiting for exactly those digs. The plant went 7 machines to 3.
//
//	2026-08-31, the same sim with the exemption scoped to excavations only.
//	Order 22 staged at Lane_08's mark again; order 23, a RETRIEVE whose §R.101
//	source hold takes Lane_08 in ModeDig for the bin one slot deeper, refused
//	`lane-held-traffic`. Order 22's own re-bind was then refused BECAUSE order 23
//	was coming for that bin — storing at the mouth would seal it in. Each was the
//	other's only releaser; the plant stopped at ~4,158 orders.
//
// The second is the first with the requester's costume changed. Scoping by what
// the row is CALLED (excavation vs source lock) rather than by what it NEEDS
// (the corridor) is what left the second one open.
//
// ── WHY THIS IS NOT A FIELD ON DigAsker ───────────────────────────────────
//
// It looks like it belongs there — DigAsker already carries the one exemption
// admitMouth honours — and folding it in would be a bug in two directions at
// once. DigAsker.Owns is read by ExcludedBy in the OPPOSITE direction ("does
// this excavation keep me out"), and by DigExclusionSQL, which is a literal
// transcription of the same two comparisons into SQL. Widening Owns would
// therefore also stop a running dig from excluding the dweller — and keeping the
// dweller out while the dig works is the whole reason the dig takes the lane
// exclusively. Two facts, two carriers.
//
// A nil set exempts nobody, so every call site that does not pass one keeps
// exactly the behaviour it had. That is the same adoption property DigAsker's
// Anyone documents for itself.
type StagedOutside map[int64]bool

// Has reports whether orderID is parked at the mark rather than in the corridor.
//
// Order id 0 is never a member: it is the zero value a failed read leaves
// behind, and a row whose owner could not be read must refuse a dig rather than
// be waved through — the same direction every other doubt in this package takes.
func (s StagedOutside) Has(orderID int64) bool {
	return orderID != 0 && s[orderID]
}

// StagedOutsideByLane carries the exemption for an acquire that spans SEVERAL
// lanes, keyed by the lane each set was resolved for.
//
// ── WHY THE KEYING EXISTS, AND IT IS NOT "ONE CORRIDOR AT A TIME" ─────────
//
// Membership is group-scoped (see StagedOutside), so within a single group the
// answer is the same for every lane and a flat set would do. The keying is here
// because AcquireLanesFor takes every lane a plan needs in ONE transaction, and
// resolvePlanLaneHolds can hand it lanes belonging to DIFFERENT GROUPS. A flat
// set across that loop would exempt an order staged in group A on a lane in
// group B, which is a corridor it has never been near. Each lane therefore
// resolves against its own group.
//
// admitMouth and DigAdmissible keep the FLAT set, because each is asked about
// exactly one lane and the caller has already resolved that lane's group. A nil
// map answers nil for every lane, which exempts nobody — the same adoption
// property the flat set has.
type StagedOutsideByLane map[int64]StagedOutside

// On returns the set for one lane, or nil when the map has nothing for it.
func (m StagedOutsideByLane) On(laneID int64) StagedOutside { return m[laneID] }

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

// Owns reports whether orderID is this asker — itself, or the order that holds
// lane locks on its behalf.
//
// IT IS THE POSITIVE SPELLING, AND IT IS THE ONE THE COMPARISON LIVES IN.
// ExcludedBy is written in terms of it rather than beside it, so the pair
// cannot drift the way the three readers above did.
//
// The two questions are duals and both are asked. ExcludedBy asks it of a DIG
// row about an order that wants the lane ("does this excavation keep me out");
// Owns asks it of an ORDINARY row about the dig being raised ("is this hold one
// of the rescue's own"). Same comparison, opposite direction of travel.
//
// orderID == 0 owns nothing: Anyone (the zero asker) must match no row, or an
// owner-blind call site would silently start exempting rows whose owner failed
// to read.
func (a DigAsker) Owns(orderID int64) bool {
	if orderID == 0 {
		return false
	}
	return orderID == a.OrderID || orderID == a.LaneOwner
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
	return !a.Owns(digOwner)
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
