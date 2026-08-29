package orders

import (
	"fmt"
	"time"

	"shingocore/store/internal/helpers"
)

// DemandRank is the plant's ranking of one demand — what the line does when two
// demands want the same thing.
//
// ── THIS IS §9'S SEAM, AND IT HAS EXACTLY TWO CALLERS ─────────────────────
//
// The owner's sentence: "one day we'll expand the demand logic from first come
// first served to like time-to-empty for demand." That day is a one-spot change
// BY CONSTRUCTION, and only if the ranking is spelled once. Its two callers are
// the scan's ORDER BY (ListAcquiring, whose SQL twin is DemandRankOrderBySQL
// below) and the under-lock outrank check in the steal. Nothing else may spell
// priority-then-oldest — or time-to-empty lands in one site and silently not the
// other, which is the failure the seam exists to make impossible.
//
// ── WHY BOTH HALVES ───────────────────────────────────────────────────────
//
// PRIORITY FIRST, and strictly: across classes a high-priority demand starves a
// low one by design, and the display is what shows it. Softening the order here
// to prevent that would hide the fact the instrument exists to report. Priority
// is a dormant column at both plants today — Edge never writes one, only the
// Core manual doors do — so the live ranking is FIFO until something else writes
// priority.
//
// THEN OLDEST, and that is what makes progress a GUARANTEE rather than a hope.
// orders.created_at is stamped once, by the INSERT, and no writer anywhere
// restamps it — not the steal's un-point, not a re-source, not a demote. So a
// demand that keeps losing a contest strictly ages toward the front of its
// class. A ranking on anything a loser's own loss could reset would let two
// demands trade the same bin forever.
type DemandRank struct {
	Priority  int
	CreatedAt time.Time
}

// Outranks reports whether a ranks ahead of b — the comparator itself.
//
// A tie is a tie: equal priority and the same instant returns false both ways,
// and the caller decides what to do with equality. The steal gate treats "does
// not outrank" as a refusal, so a tie there means the incumbent keeps the bin,
// which is the conservative half of a coin toss between two demands the plant
// cannot separate.
func (a DemandRank) Outranks(b DemandRank) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// DemandRankOrderBySQL is the comparator's SQL TWIN — the same ranking, said in
// the language the line is served in.
//
// A twin rather than a translation: TestNoThirdSpellingOfTheDemandRanking pins
// that no production query anywhere writes the ordering out by hand, so this
// fragment and Outranks above are the only two places it exists.
//
// alias is the table qualifier ("o" → "o.priority DESC, o.created_at ASC");
// empty for an unqualified query.
func DemandRankOrderBySQL(alias string) string {
	q := ""
	if alias != "" {
		q = alias + "."
	}
	return fmt.Sprintf("%spriority DESC, %screated_at ASC", q, q)
}

// LoadDemandRank resolves the DEMAND an order presents at a rank comparison.
//
// ── A LEG PRESENTS ITS PARENT'S DEMAND ────────────────────────────────────
//
// A compound child is an ordinary order row: it inherits its parent's origin but
// NOT its priority (nothing sets one, so 0) and its created_at is the moment the
// plan was written — the youngest row in the plant. Ranked on its own row a dig
// leg loses every contest it will ever have, and since the only non-zero
// priorities today belong to the hand-placed class, that hands exactly those
// orders a permanent veto over every excavation.
//
// It is right on its own terms as well as necessary: the child exists only as
// the cost of the parent's demand, so the parent's demand is what it is asking
// on behalf of.
//
// RESOLVED AT THE READ, NEVER COPIED ONTO THE CHILD. Priority lives on the
// parent's row and has one writer; a copy is a second place for it to be wrong,
// and it would go stale the moment a person raised the parent's priority.
//
// ONE LEVEL, matching laneOwnerFor's resolution of the same family. Compounds
// are one deep in this tree; a grandchild would need the walk both places, and
// making one of them deeper on its own would be the two sites disagreeing about
// what a family is.
//
// Takes a QueryRower so it can run inside the steal's transaction, which is the
// only place the answer may be read: a rank compared outside the lock is a rank
// that can be stale by the time of the grab.
func LoadDemandRank(db helpers.QueryRower, orderID int64) (DemandRank, error) {
	var r DemandRank
	err := db.QueryRow(`
		SELECT COALESCE(p.priority, o.priority), COALESCE(p.created_at, o.created_at)
		  FROM orders o
		  LEFT JOIN orders p ON p.id = o.parent_order_id
		 WHERE o.id = $1`, orderID).Scan(&r.Priority, &r.CreatedAt)
	if err != nil {
		return DemandRank{}, fmt.Errorf("load demand rank for order %d: %w", orderID, err)
	}
	return r, nil
}
