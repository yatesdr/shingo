package store

import (
	"fmt"
	"time"

	"shingocore/domain"
)

// demand_child_outcomes.go — Stage 5.2's read: what became of each episode's
// child orders.
//
// ── THE SQL IS DELIBERATELY DUMB ─────────────────────────────────────────────
//
// It groups and counts; it classifies NOTHING. The obvious shape here is a
// CASE WHEN per category returning one tidy row per origin, and it is the wrong
// shape: the classification rules — which statuses count as consumption, how a
// pre-dispatch cancel is told from a post-dispatch one, what happens to a
// status this build has never seen — would then live in a SQL string that no
// unit test can reach, in parallel with the Go that renders them.
//
// So this returns the raw (origin, status, reached-vendor) tally and
// www.ClassifyChild owns every rule, tested without Postgres. The cardinality
// that makes this affordable: at most one row per (episode × status × bool),
// and the episode set is already capped by the browser's own limit.
//
// ── WHY reached_vendor IS COMPUTED HERE AND NOT PROJECTED RAW ────────────────
//
// vendor_order_id is an opaque fleet-vendor identifier with unbounded
// cardinality; grouping by it would return one row per ORDER and defeat the
// aggregate entirely. The boolean is the only part of it this question needs,
// and collapsing it in the GROUP BY is what keeps the result set small. The
// MEANING of the boolean — that an empty vendor id proves the vendor never
// acknowledged the order — is documented at the classifier, not here.
//
// vendor_order_id is NOT NULL with an empty-string default, so the not-equal
// test is a total predicate with no three-valued-logic hole.

// ChildStatusCount is one (episode, status, reached-vendor) tally.
//
// Domain-owned for the depguard reason the whole demand grain is: www may not
// import shingocore/store, so any shape a handler names has to live where a
// handler can reach it.
type ChildStatusCount = domain.ChildStatusCount

// CountChildrenByStatus tallies child orders per episode, per status, per
// whether the fleet vendor ever acknowledged them.
//
// SCOPE MATCHES ListDemandEpisodes EXACTLY — "open, plus anything that closed
// inside the window" — and that is load-bearing rather than tidy. A narrower
// scope here would leave rows on the page whose cause column had no data
// through no fault of the data, and the page would render an absence that is an
// artefact of two queries disagreeing about what they were asked.
//
// NO LIMIT, deliberately, and it is not unbounded: the join restricts to
// episodes in the window, and the GROUP BY collapses each to at most (statuses
// × 2) rows. Adding a LIMIT would truncate a per-origin aggregate mid-origin
// and silently under-report one episode's mix — a wrong number rather than a
// missing one.
func (db *DB) CountChildrenByStatus(since time.Time) ([]ChildStatusCount, error) {
	rows, err := db.Query(`
		SELECT o.origin_id, c.status, (c.vendor_order_id <> '') AS reached_vendor,
		       COUNT(*)
		  FROM demand_origins o
		  JOIN orders c ON c.origin_id = o.origin_id
		 WHERE o.closed_at IS NULL OR o.closed_at >= $1
		 GROUP BY o.origin_id, c.status, (c.vendor_order_id <> '')`, since)
	if err != nil {
		return nil, fmt.Errorf("count children by status: %w", err)
	}
	defer rows.Close()

	var out []ChildStatusCount
	for rows.Next() {
		var c ChildStatusCount
		if err := rows.Scan(&c.OriginID, &c.Status, &c.ReachedVendor, &c.Count); err != nil {
			return nil, fmt.Errorf("scan child status count: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
