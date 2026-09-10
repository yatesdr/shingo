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
//	l1_queue_seconds       queued → dispatched      (mean,   retrieve_empty)
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
	"strings"
	"time"

	"shingocore/domain"
)

// LeadTimeRange brackets a calculate window. Inclusive of both endpoints; UTC
// is the convention (the UI converts plant-local input before passing in).
type LeadTimeRange struct {
	Start time.Time
	End   time.Time
}

// AvgL1QueueSeconds returns the mean elapsed seconds from the order's FIRST
// acquiring row → dispatched for L1 retrieve_empty orders in the window.
// payloadCode "" means all payloads.
//
// IT ENDED AT `acknowledged`, AND CORE NEVER WRITES ONE — see FlowDwellPairs in
// domain/telemetry.go for the arm and why it is dead. This helper was therefore
// structurally zero, and its zero was not harmless: the threshold calculator
// adds it to the L1 lead time, so every reorder point in the plant was computed
// as if waiting in the line took no time at all. `dispatched` is the honest end
// of that wait — the fleet call made, armor on.
//
// AND IT STARTS AT domain.AcquiringEntryStatuses, THE SAME SEAM THE DWELL PAIR
// USES. It began at `queued` alone, which is a rung an order need not ever stand
// on: a complex order is born `sourcing`, writes no queued row, and vanished
// from this mean without appearing as a gap. The whole reason this helper is
// worth getting right is that its answer becomes a reorder point, and a reorder
// point computed from a silently-narrowed population is wrong in the direction
// that under-replenishes.
func AvgL1QueueSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return avgTransition(db, domain.AcquiringEntryStatuses(), true, "dispatched", payloadCode, "retrieve_empty", r)
}

// AvgL1TransitSeconds returns the mean in_transit → delivered seconds for L1
// retrieve_empty orders.
func AvgL1TransitSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return avgTransition(db, []string{"in_transit"}, false, "delivered", payloadCode, "retrieve_empty", r)
}

// MedianL2LoadSeconds returns the median L1-delivered → confirmed seconds (the
// operator-fill window). Median, not mean, because operator fill is the only
// operator-driven segment and is exposed to long-tail outliers.
func MedianL2LoadSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return pctlTransition(db, 0.5, []string{"delivered"}, false, "confirmed", payloadCode, "retrieve_empty", r)
}

// P95MarketToCellSeconds returns the 95th-percentile in_transit → delivered
// seconds for consume-side retrieves (p95 handles reshuffle outliers).
func P95MarketToCellSeconds(db *sql.DB, payloadCode string, r LeadTimeRange) (float64, error) {
	return pctlTransition(db, 0.95, []string{"in_transit"}, false, "delivered", payloadCode, "retrieve", r)
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

// DwellPair / DwellStat / FlowDwellPairs: see domain/telemetry.go.
type (
	DwellPair = domain.DwellPair
	DwellStat = domain.DwellStat
)

// FlowDwellPairs is the standard dwell set — re-exported so callers of this
// package don't need to import domain for the common case.
func FlowDwellPairs() []DwellPair { return domain.FlowDwellPairs() }

// DwellStats returns p50, p95 and a sample count for each requested transition
// over the window. payloadCode / orderType "" mean "all".
//
// This is the per-state dwell answer — where an order's time goes between
// being asked for and being confirmed — and it needs no new query engine:
// transitionCTE below already computes any-state-to-any-state durations, and
// has had exactly one caller (the threshold calculator) since it was written.
// The four named helpers above are its other users and are untouched.
//
// One round trip per pair. At five pairs against a 3k-row orders table that is
// cheaper than the machinery to avoid it — transitionCTE runs in ~1.5ms and
// the design explicitly refused the (status, created_at) index on that basis.
//
// Inherited semantics, worth knowing before reading a number:
//
//   - MAX(from)→MAX(to) per order, so an order that visits a state twice is
//     measured across the OUTERMOST span, and one whose last to-state precedes
//     its last from-state drops out entirely (the WHERE to_ts > from_ts
//     filter). This is the existing collapse-retries behaviour, not new.
//   - The window bounds the FROM transition. A transition that starts inside
//     the window and ends after it is still counted, at its full duration.
func DwellStats(db *sql.DB, pairs []DwellPair, payloadCode, orderType string, r LeadTimeRange) ([]DwellStat, error) {
	out := make([]DwellStat, 0, len(pairs))
	for _, p := range pairs {
		cte, args := transitionCTE(p.From, p.FromEarliest, p.To, payloadCode, orderType, r)
		args = append(args, 0.5, 0.95)
		q := cte + fmt.Sprintf(`
			SELECT PERCENTILE_CONT($%d::float8) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (to_ts - from_ts))),
			       PERCENTILE_CONT($%d::float8) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (to_ts - from_ts))),
			       COUNT(*)
			FROM transitions WHERE to_ts > from_ts`, len(args)-1, len(args))

		var p50, p95 sql.NullFloat64
		var n int64
		if err := db.QueryRow(q, args...).Scan(&p50, &p95, &n); err != nil {
			return nil, fmt.Errorf("dwell %s (%v→%s): %w", p.Key, p.From, p.To, err)
		}
		out = append(out, DwellStat{
			DwellPair:  p,
			P50Seconds: p50.Float64, // NULL → 0, the package's "no signal" contract
			P95Seconds: p95.Float64,
			Count:      n,
		})
	}
	return out, nil
}

// transitionCTE builds the shared "every fromState→toState duration in the
// window" CTE plus its args in placeholder order. Callers append the final
// projection (AVG / PERCENTILE_CONT).
//
// fromStates is a SET, and fromEarliest picks which of an order's from-rows
// anchors the span. MAX(from)→MAX(to) per order is the inherited collapse and
// stays the default for every pair; the one caller that passes fromEarliest is
// the wait-in-the-line measurement, where the honest start is when the order
// JOINED the line rather than the last time it was seen standing in it. To is
// always the LAST matching row at or after the anchor.
func transitionCTE(fromStates []string, fromEarliest bool, toState, payloadCode, orderType string, r LeadTimeRange) (string, []any) {
	args := []any{toState}
	// The from-set, one placeholder each. Rendered rather than passed as an
	// array so this stays driver-agnostic — the package builds every other IN
	// list the same way.
	placeholders := make([]string, len(fromStates))
	for i, s := range fromStates {
		args = append(args, s)
		placeholders[i] = fmt.Sprintf("$%d", len(args))
	}
	fromAgg := "MAX"
	if fromEarliest {
		fromAgg = "MIN"
	}
	args = append(args, r.Start.UTC(), r.End.UTC())
	startArg, endArg := len(args)-1, len(args)
	cte := fmt.Sprintf(`WITH transitions AS (
		SELECT h_from.order_id,
		       %s(h_from.created_at) AS from_ts,
		       MAX(h_to.created_at)   AS to_ts
		FROM order_history h_from
		JOIN order_history h_to
		  ON h_to.order_id = h_from.order_id
		 AND h_to.status = $1
		 AND h_to.created_at >= h_from.created_at
		JOIN orders o ON o.id = h_from.order_id
		WHERE h_from.status IN (%s)
		  AND h_from.created_at >= $%d
		  AND h_from.created_at <= $%d`,
		fromAgg, strings.Join(placeholders, ", "), startArg, endArg)
	n := len(args)
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
func avgTransition(db *sql.DB, fromStates []string, fromEarliest bool, toState, payloadCode, orderType string, r LeadTimeRange) (float64, error) {
	cte, args := transitionCTE(fromStates, fromEarliest, toState, payloadCode, orderType, r)
	q := cte + ` SELECT AVG(EXTRACT(EPOCH FROM (to_ts - from_ts))) FROM transitions WHERE to_ts > from_ts`
	var v sql.NullFloat64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		return 0, fmt.Errorf("avg %v→%s: %w", fromStates, toState, err)
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
func pctlTransition(db *sql.DB, pctl float64, fromStates []string, fromEarliest bool, toState, payloadCode, orderType string, r LeadTimeRange) (float64, error) {
	cte, args := transitionCTE(fromStates, fromEarliest, toState, payloadCode, orderType, r)
	args = append(args, pctl)
	q := cte + fmt.Sprintf(
		` SELECT PERCENTILE_CONT($%d::float8) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (to_ts - from_ts))) FROM transitions WHERE to_ts > from_ts`,
		len(args))
	var v sql.NullFloat64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		return 0, fmt.Errorf("p%.0f %v→%s: %w", pctl*100, fromStates, toState, err)
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Float64, nil
}
