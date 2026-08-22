package orders

import (
	"database/sql"
	"fmt"
	"time"
)

// FaultStats is the /missions Faults card: what happens to a faulted order, how
// long it takes, and where faults come from.
type FaultStats struct {
	// NoticeAfterSeconds is the threshold that split replanning from fault when
	// these numbers were computed. Reported so the card can say what "notice"
	// means rather than printing a bare word.
	NoticeAfterSeconds int `json:"notice_after_seconds"`
	// Outcomes is what the faulted order did next, one row per next status,
	// with dwell percentiles for each. The open row (still faulted) is included
	// and named, because "12 faults are open right now" is the actionable one.
	Outcomes []FaultOutcome `json:"outcomes"`
	// PerDay is the daily count, split replanning vs notice. The split IS the
	// card: 730 faults a month and 24 that mattered are the same bar otherwise.
	PerDay []FaultDay `json:"per_day"`
	// ByRobot, ByNode and ByReason are the top 10 of each.
	ByRobot  []FaultGroup `json:"by_robot"`
	ByNode   []FaultGroup `json:"by_node"`
	ByReason []FaultGroup `json:"by_reason"`
}

// FaultOutcome is one next-status bucket with its dwell.
type FaultOutcome struct {
	// Status is the status the order moved to, or "" while still faulted.
	Status     string  `json:"status"`
	Count      int64   `json:"count"`
	P50Seconds float64 `json:"p50_seconds"`
	P95Seconds float64 `json:"p95_seconds"`
}

// FaultDay is one day's fault count, split by the threshold.
type FaultDay struct {
	Day        time.Time `json:"day"`
	Replanning int64     `json:"replanning"`
	Notice     int64     `json:"notice"`
}

// FaultGroup is one row of a top-N breakdown.
type FaultGroup struct {
	// Key is the robot id, node name, or vendor code.
	Key string `json:"key"`
	// Label is the human form when the key is not one — the fleet's own text
	// for a vendor code. Empty when the key speaks for itself.
	Label      string  `json:"label,omitempty"`
	Count      int64   `json:"count"`
	NoticeHits int64   `json:"notice_hits"`
	P50Seconds float64 `json:"p50_seconds"`
}

// faultCTE is the shared "every faulted row in the window, with what happened
// next" CTE.
//
// IT USES LEAD, NOT DwellStats' MAX(from)→MAX(to). That difference is the whole
// reason this query exists rather than four DwellStats pairs. DwellStats
// measures an order's OUTERMOST faulted→X span (documented at
// lead_time_queries.go), so an order that faults, recovers, faults again and
// then fails would be counted as BOTH a recovery and a failure — the outcome
// split would sum to more than the number of faults, and the recovery dwell
// would span a fault the order had already recovered from. LEAD pairs each
// faulted row with the row that actually followed it.
//
// The window bounds which FAULTED rows are counted, but the LEAD is computed
// over each order's whole history, so a fault near the window's edge still sees
// its real successor rather than a NULL. The order set is restricted to orders
// that faulted in the window so this stays a bounded scan.
//
// dwell_s is NULL for a fault that never moved. Callers substitute the live
// elapsed rather than dropping it: an order faulted for an hour with no next row
// is the most interesting row on the page, not a missing one.
const faultCTE = `
WITH windowed AS (
    SELECT DISTINCT order_id
      FROM order_history
     WHERE status = 'faulted' AND created_at >= $1 AND created_at <= $2
),
ranked AS (
    SELECT h.order_id, h.status, h.created_at, h.ref,
           o.robot_id,
           LEAD(h.status)     OVER (PARTITION BY h.order_id ORDER BY h.created_at, h.id) AS next_status,
           LEAD(h.created_at) OVER (PARTITION BY h.order_id ORDER BY h.created_at, h.id) AS next_at
      FROM order_history h
      JOIN windowed w ON w.order_id = h.order_id
      JOIN orders o    ON o.id = h.order_id
),
faults AS (
    SELECT order_id, created_at, ref, robot_id,
           COALESCE(next_status, '') AS next_status,
           COALESCE(
               EXTRACT(EPOCH FROM (next_at - created_at)),
               EXTRACT(EPOCH FROM (now() - created_at))
           ) AS dwell_s
      FROM ranked
     WHERE status = 'faulted'
       AND created_at >= $1 AND created_at <= $2
)`

// GetFaultStats computes the Faults card over [start, end].
//
// Five round trips over one CTE rather than one query returning five shapes.
// Same trade DwellStats makes and for the same reason: the machinery to avoid it
// costs more than the queries do, and each of these is a grouped aggregate over
// at most a few thousand rows.
func GetFaultStats(db *sql.DB, r LeadTimeRange, noticeAfter time.Duration) (*FaultStats, error) {
	noticeS := noticeAfter.Seconds()
	args := []any{r.Start.UTC(), r.End.UTC(), noticeS}
	out := &FaultStats{NoticeAfterSeconds: int(noticeS)}

	// Outcomes. The empty next_status is "still faulted", and it keeps its own
	// row rather than being dropped or merged into a recovery.
	rows, err := db.Query(faultCTE+`
		SELECT next_status, COUNT(*),
		       COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY dwell_s), 0),
		       COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY dwell_s), 0)
		  FROM faults GROUP BY 1 ORDER BY 2 DESC`, args[0], args[1])
	if err != nil {
		return nil, fmt.Errorf("fault outcomes: %w", err)
	}
	for rows.Next() {
		var o FaultOutcome
		if err := rows.Scan(&o.Status, &o.Count, &o.P50Seconds, &o.P95Seconds); err != nil {
			rows.Close()
			return nil, fmt.Errorf("fault outcomes scan: %w", err)
		}
		out.Outcomes = append(out.Outcomes, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fault outcomes: %w", err)
	}

	// Per day, split by the threshold.
	rows, err = db.Query(faultCTE+`
		SELECT date_trunc('day', created_at)::date,
		       COUNT(*) FILTER (WHERE dwell_s <  $3),
		       COUNT(*) FILTER (WHERE dwell_s >= $3)
		  FROM faults GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("faults per day: %w", err)
	}
	for rows.Next() {
		var d FaultDay
		if err := rows.Scan(&d.Day, &d.Replanning, &d.Notice); err != nil {
			rows.Close()
			return nil, fmt.Errorf("faults per day scan: %w", err)
		}
		out.PerDay = append(out.PerDay, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("faults per day: %w", err)
	}

	// The three top-10s. Each is the same shape over a different key, so one
	// helper runs all three — a new dimension is a line, not a function.
	groups := []struct {
		key    string
		expr   string
		label  string
		target *[]FaultGroup
	}{
		{"robot", "NULLIF(robot_id, '')", "", &out.ByRobot},
		{"node", "NULLIF(ref->>'node', '')", "", &out.ByNode},
		{"reason", "NULLIF(ref->>'vendor_code', '')", "MIN(ref->>'vendor_desc')", &out.ByReason},
	}
	for _, g := range groups {
		label := "''"
		if g.label != "" {
			label = "COALESCE(" + g.label + ", '')"
		}
		q := faultCTE + fmt.Sprintf(`
			SELECT %s AS k, %s, COUNT(*),
			       COUNT(*) FILTER (WHERE dwell_s >= $3),
			       COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dwell_s), 0)
			  FROM faults WHERE %s IS NOT NULL
			 GROUP BY k ORDER BY 3 DESC, k LIMIT 10`, g.expr, label, g.expr)
		rows, err := db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("faults by %s: %w", g.key, err)
		}
		for rows.Next() {
			var row FaultGroup
			if err := rows.Scan(&row.Key, &row.Label, &row.Count, &row.NoticeHits, &row.P50Seconds); err != nil {
				rows.Close()
				return nil, fmt.Errorf("faults by %s scan: %w", g.key, err)
			}
			*g.target = append(*g.target, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("faults by %s: %w", g.key, err)
		}
	}
	return out, nil
}
