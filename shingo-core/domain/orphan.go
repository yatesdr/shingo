// orphan.go — the read shapes behind Stage 5.7, the orphan lane and its trend.
//
// AN ORPHAN is an order that should have carried a demand origin and did not:
// origin_class = 'orphan' (migration 61). It is a reconciliation finding, never
// deleted and never auto-attached, and the plan is explicit that THE TREND is
// the number that matters — a level tells you the bucket is non-empty, which is
// almost always true and almost never actionable.
//
// Here rather than in store/ because www may not import shingocore/store.

package domain

import "time"

// OrphanBucket is one time bucket of order creation: how many orders were
// created in it, and how many of those were orphans.
//
// ── WHY BOTH NUMBERS, AND WHY THEY SHARE ONE CLOCK ───────────────────────────
//
// A bare orphan COUNT trend confounds "more orphans" with "more orders". A
// plant that doubles its output doubles its orphans at an unchanged failure
// rate, and the count line climbs while nothing has got worse. So the trend
// needs a denominator, and the denominator has to be measured on the same axis
// as the numerator or the ratio is between two different clocks.
//
// ── THE TIMESTAMP DECISION: created_at, NOT orphan_aged_at ───────────────────
//
// store.AgeOutOrphanOrders' doc comment anticipates the Phase 6 trend coming
// off orphan_aged_at. THAT EXPECTATION IS WRONG, and this is the record of why.
// Four reasons, the first of which is on its own decisive:
//
//  1. THE RATE HAS NO DENOMINATOR ON THAT AXIS. orphan_aged_at exists only on
//     orphans that have already aged out. There is no such thing as "all orders
//     aged at time T" — non-orphans never get the column at all. So a numerator
//     keyed on orphan_aged_at can only be divided by a denominator keyed on
//     created_at, which is a ratio between two different clocks, or by nothing
//     at all, which is the confounded bare count above.
//
//  2. IT IS BLIND TO THE RECENT PAST BY CONSTRUCTION. AgeOutOrphanOrders stamps
//     only rows with created_at < now − grace, so every orphan younger than the
//     grace period has orphan_aged_at IS NULL. The most recent buckets — exactly
//     the ones that would show a rate that has just started climbing — are
//     structurally empty. A trend that cannot see the present looks like a
//     trend, which makes it worse than no trend.
//
//  3. IT IS THE SWEEP'S CLOCK, NOT THE ORDER'S. Every orphan the sweep retires
//     in one pass is stamped at one instant, so the shape of the line is the
//     shape of the sweep's schedule. Widen the ticker and the "trend" changes
//     with nothing on the floor having moved.
//
//  4. A SWEEP OUTAGE MOVES THE LINE. A reconciler down for a day puts zero in
//     that day's buckets and a spike in the next one. created_at is stamped once
//     by the order's own creation and is never restamped by anything, which is
//     what makes it immune.
//
// orphan_aged_at keeps its real job, which is the fresh/aged split on
// OrphanSite — "is anyone still being asked about this one". That is a question
// about the sweep, and the sweep's clock is the right clock to answer it with.
type OrphanBucket struct {
	// Start is the bucket's left edge, keyed on orders.created_at.
	Start time.Time

	// Orphans is orders created in this bucket with origin_class = 'orphan'.
	// A REAL MEASURED COUNT including zero: a bucket that had orders and none
	// of them orphaned is the healthy reading and must render as 0, not as an
	// absence.
	Orphans int

	// Orders is EVERY order created in this bucket — the denominator.
	//
	// ZERO IS REACHABLE AND IS NOT A ZERO RATE. A quiet hour, a shutdown, a
	// weekend: the bucket holds no rows, so the orphan rate is unmeasured
	// rather than 0%. The style guide files "the window holds no rows"
	// explicitly under NO DATA, and 0% is the reassuring reading a dead feed
	// would also produce.
	Orders int
}

// OrphanSite is the orphan bucket for one station — the "per site" half of the
// plan row.
//
// The live/aged split is derived from orphan_aged_at IS NULL rather than stored
// as a class, because origin_class already answers a different question — how
// this order related to a demand AT CREATE TIME, a fact that stays true forever
// — and a sweep that rewrote it would overwrite a fact with a derivation.
type OrphanSite struct {
	StationID string

	// Live is findings nobody has stopped asking about. These are the
	// actionable ones.
	Live int

	// Aged is findings the sweep has retired. Still recorded, never deleted:
	// the bucket is the evidence that a reconciliation gap existed.
	Aged int

	Total int

	// OldestLive is when the oldest still-live finding was created, or nil when
	// there are none. NIL IS NOT A ZERO TIME — a station with no live orphans
	// and a station whose oldest live orphan arrived at the epoch are different
	// readings, and time.Time's zero value cannot tell them apart.
	OldestLive *time.Time
}
