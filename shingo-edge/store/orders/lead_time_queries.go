// lead_time_queries.go — order_history-derived lead-time helpers for
// the UOP-threshold replenishment calculator.
//
// v6: the calculator runs on demand (engineer-triggered Calculate
// button), so these helpers take an explicit date range rather than a
// lookback in days. The legacy lookback-days variants are kept for the
// fallback path that runs without a date picker.
//
// State-transition pairs used:
//
//   l1_queue_seconds       queued → acknowledged           (mean)
//   l1_transit_seconds     in_transit → delivered          (mean, L1 retrieve_empty)
//   l2_load_seconds        L1's delivered → confirmed      (median)
//   l2_transit_seconds     in_transit → delivered          (mean, L2 store-out)
//   market_to_cell_seconds in_transit → delivered          (p95, consume retrieve)
//
// Choice of central-tendency per signal:
//   - L1 queue / L1 transit / L2 transit are robot-driven and tightly
//     distributed; mean is a fine estimator and the existing data set
//     is small enough that a percentile would be noisy.
//   - L2 load is operator-driven and exposed to long-tail outliers
//     (end-of-shift confirms, weekend confirms, walked-away-from-
//     station, Core's reconciler auto-confirming a stuck-delivered
//     order). Median lets every outlier class fall out without
//     filtering by a magic detail string.
//   - Market→cell has reshuffle outliers (rare long retrieves from
//     blocked lanes). p95 captures the conservative tail.
//
// MAX(created_at) per (order_id, status) so error/retry transitions
// don't double-count.
//
// All helpers return 0 when there is no data in the window; callers
// (the calculator) treat 0 as "no signal" and surface a LOW confidence
// score rather than baking the absence into a real value.

package orders

import (
	"database/sql"
	"fmt"
	"time"
)

// DateRange brackets the window for a calculate run. Inclusive of both
// endpoints. UTC is the convention; the UI converts plant-local input
// before passing in.
type DateRange struct {
	Start time.Time
	End   time.Time
}

func (r DateRange) startISO() string { return r.Start.UTC().Format("2006-01-02 15:04:05") }
func (r DateRange) endISO() string   { return r.End.UTC().Format("2006-01-02 15:04:05") }

// AvgL1QueueSeconds returns the mean elapsed seconds from queued →
// acknowledged for L1 retrieve_empty orders in the given window.
// payloadCode = "" means "across all payloads".
func AvgL1QueueSeconds(db *sql.DB, payloadCode string, r DateRange) (float64, error) {
	return avgStateTransition(db, "queued", "acknowledged", payloadCode, r, "retrieve_empty")
}

// AvgL1TransitSeconds returns the mean elapsed seconds from
// in_transit → delivered for L1 retrieve_empty orders.
func AvgL1TransitSeconds(db *sql.DB, payloadCode string, r DateRange) (float64, error) {
	return avgStateTransition(db, "in_transit", "delivered", payloadCode, r, "retrieve_empty")
}

// MedianL2LoadSeconds returns the median elapsed seconds from L1's
// delivered → confirmed (operator fill window). Median (not mean)
// because operator-fill is the only operator-driven segment in the
// calculator and is exposed to long-tail outliers: end-of-shift
// confirms, weekend confirms, walked-away-from-station, Core's
// reconciler auto-confirming a stuck-delivered order. A median lets
// all those outlier classes fall out without filtering by a magic
// detail string — and the magic-string filter only caught one class
// anyway.
func MedianL2LoadSeconds(db *sql.DB, payloadCode string, r DateRange) (float64, error) {
	return medianStateTransition(db, "delivered", "confirmed", payloadCode, r, "retrieve_empty")
}

// AvgL2TransitSeconds returns the mean elapsed seconds from
// in_transit → delivered for L2 store / store-out orders.
func AvgL2TransitSeconds(db *sql.DB, payloadCode string, r DateRange) (float64, error) {
	return avgStateTransition(db, "in_transit", "delivered", payloadCode, r, "store")
}

// P95MarketToCellSeconds returns the 95th-percentile retrieve duration
// (in_transit → delivered) for consume-side retrieves. p95 (not mean)
// handles reshuffle outliers, per the v6 brief's calculator spec.
//
// SQLite doesn't have a percentile aggregate. Approach: query all
// observed durations in the window and pick the 95th-percentile rank.
// For a typical window of 14 days × ~50 cycles/day = ~700 rows; cheap.
func P95MarketToCellSeconds(db *sql.DB, payloadCode string, r DateRange) (float64, error) {
	args := []any{r.startISO(), r.endISO(), "retrieve"}
	payloadFilter := ""
	if payloadCode != "" {
		payloadFilter = " AND o.payload_code = ?"
		args = append(args, payloadCode)
	}
	rows, err := db.Query(`
		WITH transitions AS (
			SELECT
				h_from.order_id,
				MAX(h_from.created_at) AS from_ts,
				MAX(h_to.created_at)   AS to_ts
			FROM order_history h_from
			JOIN order_history h_to
			  ON h_to.order_id = h_from.order_id
			 AND h_to.new_status = 'delivered'
			 AND h_to.created_at >= h_from.created_at
			JOIN orders o ON o.id = h_from.order_id
			WHERE h_from.new_status = 'in_transit'
			  AND h_from.created_at >= ?
			  AND h_from.created_at <= ?
			  AND o.order_type = ?
			`+payloadFilter+`
			GROUP BY h_from.order_id
		)
		SELECT (julianday(to_ts) - julianday(from_ts)) * 86400.0
		FROM transitions
		WHERE to_ts > from_ts
		ORDER BY 1 ASC`, args...)
	if err != nil {
		return 0, fmt.Errorf("p95 market_to_cell: %w", err)
	}
	defer rows.Close()
	var durations []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		durations = append(durations, d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(durations) == 0 {
		return 0, nil
	}
	// Index for p95 on a sorted slice: ceil(0.95 * n) - 1.
	idx := int(0.95*float64(len(durations))) // floor; for small n this lands at last idx-1
	if idx >= len(durations) {
		idx = len(durations) - 1
	}
	return durations[idx], nil
}

// CountCompletedOrdersInWindow returns how many orders of orderType
// for payloadCode landed at the given terminal status (confirmed for
// L1, delivered for L2 transit, etc.) inside the window. Used by the
// calculator's confidence-score: HIGH/MEDIUM/LOW depends on data
// coverage.
func CountCompletedOrdersInWindow(db *sql.DB, orderType, terminalStatus, payloadCode string, r DateRange) (int, error) {
	args := []any{terminalStatus, r.startISO(), r.endISO(), orderType}
	payloadFilter := ""
	if payloadCode != "" {
		payloadFilter = " AND o.payload_code = ?"
		args = append(args, payloadCode)
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(DISTINCT o.id)
		FROM orders o
		JOIN order_history h ON h.order_id = o.id
		WHERE h.new_status = ?
		  AND h.created_at >= ?
		  AND h.created_at <= ?
		  AND o.order_type = ?`+payloadFilter, args...).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// transitionQuery builds the shared "every fromState→toState duration
// in the window" CTE + final SELECT. Returns the query string and the
// args slice in placeholder order; callers append the final select-
// projection (AVG(...) for mean, ORDER BY for median).
func transitionQuery(fromState, toState, payloadCode string, r DateRange, orderType string) (string, []any) {
	args := []any{toState, fromState, r.startISO(), r.endISO()}
	payloadFilter := ""
	if payloadCode != "" {
		payloadFilter = " AND o.payload_code = ?"
		args = append(args, payloadCode)
	}
	typeFilter := ""
	if orderType != "" {
		typeFilter = " AND o.order_type = ?"
		args = append(args, orderType)
	}
	q := `
		WITH transitions AS (
			SELECT
				h_from.order_id,
				MAX(h_from.created_at) AS from_ts,
				MAX(h_to.created_at)   AS to_ts
			FROM order_history h_from
			JOIN order_history h_to
			  ON h_to.order_id = h_from.order_id
			 AND h_to.new_status = ?
			 AND h_to.created_at >= h_from.created_at
			JOIN orders o ON o.id = h_from.order_id
			WHERE h_from.new_status = ?
			  AND h_from.created_at >= ?
			  AND h_from.created_at <= ?
			` + payloadFilter + typeFilter + `
			GROUP BY h_from.order_id
		)`
	return q, args
}

// avgStateTransition returns the mean elapsed seconds between
// successive order_history rows of (old_status=fromState,
// new_status=toState) for orders matching the orderType + payloadCode
// filter inside the window.
func avgStateTransition(db *sql.DB, fromState, toState, payloadCode string, r DateRange, orderType string) (float64, error) {
	q, args := transitionQuery(fromState, toState, payloadCode, r, orderType)
	query := q + `
		SELECT AVG((julianday(to_ts) - julianday(from_ts)) * 86400.0)
		FROM transitions
		WHERE to_ts > from_ts`

	var avgVal sql.NullFloat64
	if err := db.QueryRow(query, args...).Scan(&avgVal); err != nil {
		return 0, fmt.Errorf("avg %s→%s: %w", fromState, toState, err)
	}
	if !avgVal.Valid {
		return 0, nil
	}
	return avgVal.Float64, nil
}

// medianStateTransition returns the median elapsed seconds. SQLite
// has no median aggregate; pull every duration and pick the middle
// after sorting. The typical window (14 days × ~50 cycles/day ≈ 700
// rows) is well within "fine to scan in memory" territory.
//
// For even-length samples the median is the mean of the two middle
// values; matches the conventional statistical definition.
func medianStateTransition(db *sql.DB, fromState, toState, payloadCode string, r DateRange, orderType string) (float64, error) {
	q, args := transitionQuery(fromState, toState, payloadCode, r, orderType)
	query := q + `
		SELECT (julianday(to_ts) - julianday(from_ts)) * 86400.0
		FROM transitions
		WHERE to_ts > from_ts
		ORDER BY 1 ASC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return 0, fmt.Errorf("median %s→%s: %w", fromState, toState, err)
	}
	defer rows.Close()
	var durations []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		durations = append(durations, d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := len(durations)
	if n == 0 {
		return 0, nil
	}
	if n%2 == 1 {
		return durations[n/2], nil
	}
	return (durations[n/2-1] + durations[n/2]) / 2, nil
}
