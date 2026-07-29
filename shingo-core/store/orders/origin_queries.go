// origin_queries.go — reading orders by the demand episode that caused them.
//
// The demand grain has two directions and until now only one of them was
// readable. ListDemandEpisodes counts children per episode (a correlated
// COUNT(*)), which answers "how many"; nothing answered "which ones". The
// origin-indexed forensics surface is the other direction: given one demand,
// every order it spawned, in the order they happened.
//
// The join is orders.origin_id, which is STAMPED FORWARD at the create site and
// never reconstructed by walking parent_order_id — see domain.Order's comment on
// the field for why that walk dead-ends. This query therefore reaches a compound
// child directly, without knowing anything about compound structure.

package orders

import (
	"database/sql"
	"fmt"
)

// ListByOrigin returns every order stamped with originID, OLDEST FIRST, capped
// at limit. The bool reports whether the cap bit.
//
// ORDER IS ASCENDING, which is the opposite of every other order listing in this
// package and is deliberate. The admin listings answer "what is happening now",
// so newest-first is right for them. This one answers "what did this demand
// cause", which is a story with a beginning — read forward from the moment the
// place said it needed material, or the first order (the one whose lateness
// explains the rest) sits at the bottom of the page.
//
// id ASC IS A REAL TIEBREAK, NOT DECORATION. created_at is written from
// clock.Now() rather than the database default (see Create), and under the
// simulator's fast-forward clock several orders genuinely share a timestamp. A
// bare ORDER BY created_at leaves those in whatever order the scan returns them,
// which makes a forensic page non-deterministic exactly on the stack of orders
// that were minted together — the case someone is reading it for.
//
// THE CAP IS REPORTED, NOT SILENT, for the same reason ListDemandEpisodes
// reports its own: a truncated list rendered as though it were complete is a
// page that lies about what a demand cost. Cardinality here is UNMEASURED at a
// plant — the simulator's mean is 3.36 children with a max of 13, which is a
// property of demo.yaml's tick rates and not a bound on anything.
//
// Runs on idx_orders_origin_id, the partial index over origin_id WHERE origin_id
// IS NOT NULL declared in the baseline DDL. An empty originID would scan the
// table off the index's own predicate, so it is rejected rather than issued:
// "no origin" is not an episode and has no orders by definition.
func ListByOrigin(db *sql.DB, originID string, limit int) (out []*Order, truncated bool, err error) {
	if originID == "" {
		return nil, false, fmt.Errorf("list orders by origin: empty origin id")
	}
	if limit <= 0 {
		limit = 500
	}

	// limit+1 so the cap can be REPORTED rather than inferred. Asking for
	// exactly limit rows leaves "got limit rows" ambiguous between a full page
	// and a truncated one — ListDemandEpisodes uses the same construction.
	rows, err := db.Query(`SELECT `+SelectCols+`
		  FROM orders
		 WHERE origin_id = $1
		 ORDER BY created_at ASC, id ASC
		 LIMIT $2`, originID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list orders by origin %s: %w", originID, err)
	}
	defer rows.Close()

	got, err := ScanOrders(rows)
	if err != nil {
		return nil, false, fmt.Errorf("scan orders by origin %s: %w", originID, err)
	}
	if len(got) > limit {
		return got[:limit], true, nil
	}
	return got, false, nil
}
