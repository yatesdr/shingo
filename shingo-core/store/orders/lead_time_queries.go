// lead_time_queries.go — order_history-derived lead-time helpers for the
// UOP-threshold replenishment calculator, ported to Core (loader refactor:
// thresholds and their calculator move to Core-owned config).
//
// The Edge original (shingo-edge/store/orders/lead_time_queries.go) reads the
// Edge SQLite with julianday() duration math and pulls median/p95 samples into
// Go (SQLite has no percentile aggregate). This Postgres port uses
// EXTRACT(EPOCH FROM ...) for durations and PERCENTILE_CONT in-SQL — the same
// idiom Core already uses for ETA medians (dispatch/eta/medians.go). The
// state-transition pairs, central-tendency choices per signal, and the
// "0 = no signal in the window" contract are IDENTICAL to the Edge; see that
// file's header for the rationale.
//
// State-transition pairs:
//
//	l1_queue_seconds       queued → acknowledged    (mean,   retrieve_empty)
//	l1_transit_seconds     in_transit → delivered   (mean,   retrieve_empty)
//	l2_load_seconds        delivered → confirmed    (median, retrieve_empty)
//	market_to_cell_seconds in_transit → delivered   (p95,    retrieve)
//
// Column note: Core order_history names the status column `status`; the Edge
// names it `new_status`. MAX(created_at) per (order_id, transition) so error/
// retry transitions don't double-count, matching the Edge.

package orders

import (
	"database/sql"
	"fmt"
	"time"
)

// LeadTimeRange brackets a calculate window. Inclusive of both endpoints; UTC
// is the convention (the UI converts plant-local input before passing in).
type LeadTimeRange struct {
	Start time.Time
	End   time.Time
}

// AvgL1QueueSeconds returns the mean elapsed seconds from queued → acknowledged
// for L1 retrieve_empty orders in the window. payloadCode "" means all payloads.
func AvgL1QueueSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return avgTransition(db, "queued", "acknowledged", payloadCode, "retrieve_empty", r)
}

// AvgL1TransitSeconds returns the mean in_transit → delivered seconds for L1
// retrieve_empty orders.
func AvgL1TransitSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return avgTransition(db, "in_transit", "delivered", payloadCode, "retrieve_empty", r)
}

// MedianL2LoadSeconds returns the median L1-delivered → confirmed seconds (the
// operator-fill window). Median, not mean, because operator fill is the only
// operator-driven segment and is exposed to long-tail outliers.
func MedianL2LoadSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return pctlTransition(db, 0.5, "delivered", "confirmed", payloadCode, "retrieve_empty", r)
}

// P95MarketToCellSeconds returns the 95th-percentile in_transit → delivered
// seconds for consume-side retrieves (p95 handles reshuffle outliers).
func P95MarketToCellSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return pctlTransition(db, 0.95, "in_transit", "delivered", payloadCode, "retrieve", r)
}

// CountCompletedOrdersInWindow returns how many distinct orders of orderType
// for payloadCode reached terminalStatus inside the window — the calculator's
// confidence-score coverage signal.
func CountCompletedOrdersInWindow(db *sql.DB, orderType, terminalStatus, payloadCode string, r LeadTimeRange) (int, error) {
	q := `SELECT COUNT(DISTINCT o.id)
		FROM orders o
		JOIN order_history h ON h.order_id = o.id
		WHERE h.status = $1 AND h.created_at >= $2 AND h.created_at <= $3 AND o.order_type = $4`
	args := []any{terminalStatus, r.Start.UTC(), r.End.UTC(), orderType}
	if payloadCode != "" {
		q += " AND o.payload_code = $5"
		args = append(args, payloadCode)
	}
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s/%s: %w", orderType, terminalStatus, err)
	}
	return n, nil
}

// transitionCTE builds the shared "every fromState→toState duration in the
// window" CTE plus its args in placeholder order. Callers append the final
// projection (AVG / PERCENTILE_CONT). MAX(created_at) per order collapses
// retry transitions, matching the Edge.
func transitionCTE(fromState, toState, payloadCode, orderType string, r LeadTimeRange) (string, []any) {
	args := []any{toState, fromState, r.Start.UTC(), r.End.UTC()}
	cte := `WITH transitions AS (
		SELECT h_from.order_id,
		       MAX(h_from.created_at) AS from_ts,
		       MAX(h_to.created_at)   AS to_ts
		FROM order_history h_from
		JOIN order_history h_to
		  ON h_to.order_id = h_from.order_id
		 AND h_to.status = $1
		 AND h_to.created_at >= h_from.created_at
		JOIN orders o ON o.id = h_from.order_id
		WHERE h_from.status = $2
		  AND h_from.created_at >= $3
		  AND h_from.created_at <= $4`
	n := 4
	if payloadCode != "" {
		n++
		cte += fmt.Sprintf(" AND o.payload_code = $%d", n)
		args = append(args, payloadCode)
	}
	if orderType != "" {
		n++
		cte += fmt.Sprintf(" AND o.order_type = $%d", n)
		args = append(args, orderType)
	}
	cte += " GROUP BY h_from.order_id)"
	return cte, args
}

// avgTransition returns the mean transition duration in seconds, 0 when the
// window has no qualifying transitions.
func avgTransition(db *sql.DB, fromState, toState, payloadCode, orderType string, r LeadTimeRange) (float64, error) {
	cte, args := transitionCTE(fromState, toState, payloadCode, orderType, r)
	q := cte + ` SELECT AVG(EXTRACT(EPOCH FROM (to_ts - from_ts))) FROM transitions WHERE to_ts > from_ts`
	var v sql.NullFloat64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		return 0, fmt.Errorf("avg %s→%s: %w", fromState, toState, err)
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Float64, nil
}

// pctlTransition returns the given continuous percentile (0..1) of the
// transition durations in seconds, 0 when the window is empty. The fraction is
// bound as a parameter (never interpolated) and cast so the planner sees a
// concrete float8.
func pctlTransition(db *sql.DB, pctl float64, fromState, toState, payloadCode, orderType string, r LeadTimeRange) (float64, error) {
	cte, args := transitionCTE(fromState, toState, payloadCode, orderType, r)
	args = append(args, pctl)
	q := cte + fmt.Sprintf(
		` SELECT PERCENTILE_CONT($%d::float8) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (to_ts - from_ts))) FROM transitions WHERE to_ts > from_ts`,
		len(args))
	var v sql.NullFloat64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		return 0, fmt.Errorf("p%.0f %s→%s: %w", pctl*100, fromState, toState, err)
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Float64, nil
}
