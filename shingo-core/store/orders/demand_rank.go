package orders

import (
	"fmt"
	"time"

	"shingocore/store/internal/helpers"
)

// DemandRank is the plant's ranking of one demand: priority first, then oldest.
//
// §9's SEAM, WITH EXACTLY TWO CALLERS — the scan's ORDER BY (ListAcquiring, via
// the SQL twin below) and the steal's under-lock outrank check. The ranking is
// expected to become time-to-empty, which is a one-spot change only while it is
// spelled once; a third spelling is a site that change would silently not reach.
// TestNoThirdSpellingOfTheDemandRanking guards it.
//
// PRIORITY FIRST, strictly: a high-priority demand starves a low one by design,
// and the display is what reports it. Priority is a dormant column at both
// plants — Edge writes none, only the Core manual doors do — so the live ranking
// is FIFO until something else writes one.
//
// THEN OLDEST, which is what makes progress a guarantee. created_at is stamped
// once by the INSERT and no writer restamps it, so a demand that keeps losing
// ages toward the front of its class rather than trading the same bin forever.
type DemandRank struct {
	Priority  int
	CreatedAt time.Time
	// ID breaks an exact (priority, created_at) tie so the ranking is TOTAL.
	// Only Precedes reads it; see both methods below.
	ID int64
}

// Outranks reports whether a ranks strictly ahead of b on the demand itself.
//
// A TIE RETURNS FALSE BOTH WAYS, and that is what the steal gate wants: it
// treats "does not outrank" as a refusal, so two demands the plant cannot
// separate leave the bin with the incumbent rather than taking it from each
// other. Adding the id tiebreak here would make an arbitrary row id decide who
// gets dug out from under whom.
func (a DemandRank) Outranks(b DemandRank) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// Precedes is the TOTAL order: Outranks, then the older row id.
//
// The line needs one and the contest does not. An ORDER BY that stops at
// (priority, created_at) leaves tied rows in whatever order Postgres returns
// them, so the scan can hand back a different sequence per call for the same
// board — and the pairwise twin check has no answer for a pair neither side
// outranks. Ids ascend with creation, so the tiebreak agrees with the ageing
// guarantee rather than cutting across it.
func (a DemandRank) Precedes(b DemandRank) bool {
	if a.Priority != b.Priority || !a.CreatedAt.Equal(b.CreatedAt) {
		return a.Outranks(b)
	}
	return a.ID < b.ID
}

// DemandRankOrderBySQL is Precedes' SQL twin — the same total order in the
// language the line is served in. It and the two methods above are the only
// places the ordering exists.
//
// NO ALIAS PARAMETER. It had one, and one caller passing "": every reader of
// this ordering selects from `orders` unqualified, so the qualified branch was
// shaped for a second caller that does not exist and was never exercised.
func DemandRankOrderBySQL() string {
	return "priority DESC, created_at ASC, id ASC"
}

// LoadDemandRank resolves the demand an order presents at a rank comparison.
//
// A COMPOUND CHILD PRESENTS ITS PARENT'S. A leg inherits its parent's origin but
// not its priority (nothing sets one, so 0), and its created_at is the moment
// the plan was written — the youngest row in the plant. Ranked on its own row it
// loses every contest, and since the only non-zero priorities today belong to
// the hand-placed class, that would give those orders a veto over every
// excavation. It is also correct on its own terms: the child exists as the cost
// of the parent's demand.
//
// RESOLVED AT THE READ, NEVER COPIED ONTO THE CHILD: priority has one writer,
// and a copy goes stale the moment somebody raises the parent's.
//
// One level, matching laneOwnerFor's resolution of the same family.
//
// Takes a QueryRower so it runs inside the steal's transaction — a rank compared
// outside the lock can be stale by the time of the grab.
func LoadDemandRank(db helpers.QueryRower, orderID int64) (DemandRank, error) {
	var r DemandRank
	// The ID travels with the demand it belongs to — the PARENT's for a leg, like
	// the other two columns — so Precedes breaks a tie between the two demands
	// rather than between two pieces of paperwork.
	err := db.QueryRow(`
		SELECT COALESCE(p.priority, o.priority), COALESCE(p.created_at, o.created_at),
		       COALESCE(p.id, o.id)
		  FROM orders o
		  LEFT JOIN orders p ON p.id = o.parent_order_id
		 WHERE o.id = $1`, orderID).Scan(&r.Priority, &r.CreatedAt, &r.ID)
	if err != nil {
		return DemandRank{}, fmt.Errorf("load demand rank for order %d: %w", orderID, err)
	}
	return r, nil
}
