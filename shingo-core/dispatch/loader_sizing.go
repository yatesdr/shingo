package dispatch

import "fmt"

// loader_sizing.go — how many carriers a loader needs to reach its threshold.
//
// This is a port of the sizing that used to live in the Edge's
// HandleLoopBelowThreshold (shingo-edge/engine/operator_demand_loader.go),
// carried over unchanged — including the negative-current clamp — when Core
// took over the whole decision. The Edge original is deleted (2026-08-02,
// with the Edge's half of replenishment), so this file is now the only copy;
// the golden vectors that pinned the two halves against each other retired
// with it.
//
// The design notes for this work call the function "sizeAsk". No such function
// existed on the Edge — it was an inline block, and its guard on per-bin capacity
// sat OUTSIDE that block, in the caller. Naming it here and pulling the guard
// in with it was the point of porting rather than copying: on Core the
// precondition and the arithmetic cannot drift apart, because a caller cannot
// reach the arithmetic without passing through the precondition.

// SizingOutcome says what the sizing decided and why. Every value is a normal
// answer — none of them is an error — but they are distinguished because the
// caller logs them and because two of them mean "create nothing", which is a
// result a reader will otherwise mistake for a failure.
type SizingOutcome string

const (
	// SizingOK: a positive number of carriers is wanted.
	SizingOK SizingOutcome = "ok"
	// SizingAtThreshold: the loop is already at or above its threshold. Nothing
	// to do, and nothing wrong.
	SizingAtThreshold SizingOutcome = "at_threshold"
	// SizingNoCapacity: the payload has no per-bin capacity in the catalog, so
	// "how many bins" has no answer. A configuration error, not a demand signal.
	SizingNoCapacity SizingOutcome = "no_per_bin_capacity"
)

// BinsToReachThreshold answers how many full carriers of a payload it takes to
// bring a loop from currentUOP up to threshold, given how many units of that
// payload fit in one carrier.
//
// perBinCapacity comes from the payload catalog, never from the loader: a
// supermarket-side loader carries capacity 0 because it consumes nothing itself,
// while the units-per-carrier figure is a property of the part. Zero or negative
// capacity is a configuration error and produces SizingNoCapacity. Falling back
// to counting bins instead would reintroduce the fixed floor that made loaders
// over-order in the first place — the reason this path exists.
//
// A NEGATIVE currentUOP is a broken ledger, not demand for the entire threshold.
// Springfield reported -443, which made the gap 455 and the answer 26 carriers:
// every number downstream computed from a reading already known to be garbage.
// The gap is sized from 0 instead.
//
// ORDERING STILL CONTINUES on a negative reading. A negative total means
// material moved off the books, which is exactly when the loop needs
// replenishing. A broken count does not get to SIZE the order; it does not get
// to cancel it either.
//
// What comes back is a CEILING, not a decision. It is what the loop needs, with
// no knowledge of what is already on its way or how many free windows exist —
// both of those bound it later, at the window budget. Callers must not treat
// this number as the count to create.
func BinsToReachThreshold(threshold, currentUOP, perBinCapacity int) (bins int, outcome SizingOutcome, detail string) {
	if perBinCapacity <= 0 {
		return 0, SizingNoCapacity, fmt.Sprintf("payload has no per-bin capacity (uop_capacity=%d)", perBinCapacity)
	}
	current := currentUOP
	if current < 0 {
		current = 0
	}
	gap := threshold - current
	if gap <= 0 {
		return 0, SizingAtThreshold, fmt.Sprintf("currentUOP=%d already meets threshold=%d", currentUOP, threshold)
	}
	// Round up: a partial carrier's worth of shortfall still needs a carrier.
	return (gap + perBinCapacity - 1) / perBinCapacity, SizingOK, ""
}

// NegativeCurrentUOP reports whether a reading was clamped by the sizing above.
// Split out so the caller can log the clamp without re-deriving the rule, and so
// a change to what counts as a broken ledger happens in one place.
func NegativeCurrentUOP(currentUOP int) bool { return currentUOP < 0 }
