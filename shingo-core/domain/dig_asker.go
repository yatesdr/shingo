package domain

// dig_asker.go — the order a dig-lock question is asked on behalf of.
//
// MOVED FROM store/reservations (B1). The planner and its parts (binresolver,
// binsource, the source finder) carry the asker through every question and must
// not import store/reservations, which holds the writers (the planner-purity
// depguard rule). The type and its Go answers live here; reservations keeps
// aliases for every caller, and DigExclusionSQL, the SQL twin of ExcludedBy,
// stays in store/reservations/dig_exclusion.go beside the rows it reads.

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

// Args returns the two bind values reservations.DigExclusionSQL's placeholders take, in the
// order the placeholders were named. Kept beside the renderer so a caller
// cannot pass them in the wrong order or forget one.
func (a DigAsker) Args() []any { return []any{a.OrderID, a.LaneOwner} }
