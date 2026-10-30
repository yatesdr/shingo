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
// on: an order its first hand takes goes pending → dispatched and writes no
// queued row (and complex orders, born `sourcing` before C1, wrote none either),
// so it would vanish from this mean without appearing as a gap. The whole reason this helper is
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

// DwellPair / DwellStat: see domain/telemetry.go.
type (
	DwellPair = domain.DwellPair
	DwellStat = domain.DwellStat
)

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
	return DwellStatsScoped(db, pairs, OrderScope{PayloadCode: payloadCode, OrderType: orderType}, r)
}

// OrderScope narrows a lead-time read to some orders. An empty field means
// "all". StationID and RobotID are the orders' own station_id and robot_id,
// the same columns the missions list filters on.
type OrderScope struct {
	PayloadCode string
	OrderType   string
	StationID   string
	RobotID     string
}

// DwellStatsScoped is DwellStats over the orders in scope.
func DwellStatsScoped(db *sql.DB, pairs []DwellPair, scope OrderScope, r LeadTimeRange) ([]DwellStat, error) {
	out := make([]DwellStat, 0, len(pairs))
	for _, p := range pairs {
		cte, args := transitionCTEScoped(p.From, p.FromEarliest, p.To, scope, r)
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
	return transitionCTEScoped(fromStates, fromEarliest, toState, OrderScope{PayloadCode: payloadCode, OrderType: orderType}, r)
}

// transitionCTEScoped is transitionCTE over the orders in scope.
func transitionCTEScoped(fromStates []string, fromEarliest bool, toState string, scope OrderScope, r LeadTimeRange) (string, []any) {
	body, args := transitionsSQL("transitions", fromStates, fromEarliest, toState, scope, r, false, nil)
	return "WITH " + body, args
}

// transitionsSQL is one named transitions CTE (`name AS (...)`), its
// placeholders numbered after args, which it returns extended. byPayload
// carries each order's payload_code beside its span, so one statement can
// aggregate every payload (LeadTimesByPayload) under the same pairing rules
// the per-payload helpers use.
func transitionsSQL(name string, fromStates []string, fromEarliest bool, toState string, scope OrderScope, r LeadTimeRange, byPayload bool, args []any) (string, []any) {
	args = append(args, toState)
	toArg := len(args)
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
	payloadCol, payloadGroup := "", ""
	if byPayload {
		payloadCol, payloadGroup = "o.payload_code, ", ", o.payload_code"
	}
	cte := fmt.Sprintf(`%s AS (
		SELECT h_from.order_id, %s
		       %s(h_from.created_at) AS from_ts,
		       MAX(h_to.created_at)   AS to_ts
		FROM order_history h_from
		JOIN order_history h_to
		  ON h_to.order_id = h_from.order_id
		 AND h_to.status = $%d
		 AND h_to.created_at >= h_from.created_at
		JOIN orders o ON o.id = h_from.order_id
		WHERE h_from.status IN (%s)
		  AND h_from.created_at >= $%d
		  AND h_from.created_at <= $%d`,
		name, payloadCol, fromAgg, toArg, strings.Join(placeholders, ", "), startArg, endArg)
	for _, c := range []struct{ col, val string }{
		{"payload_code", scope.PayloadCode},
		{"order_type", scope.OrderType},
		{"station_id", scope.StationID},
		{"robot_id", scope.RobotID},
	} {
		if c.val != "" {
			args = append(args, c.val)
			cte += fmt.Sprintf(" AND o.%s = $%d", c.col, len(args))
		}
	}
	cte += " GROUP BY h_from.order_id" + payloadGroup + ")"
	return cte, args
}

// LeadTimeInputs are the calculator's observed inputs for one payload over a
// window, as the four helpers above and CountCompletedOrdersInWindow compute
// them one payload at a time, plus the median measured consumption rate.
type LeadTimeInputs struct {
	L1QueueSeconds      float64
	L1TransitSeconds    float64
	L2LoadSeconds       float64
	MarketToCellSeconds float64
	SamplesL1           int
	SamplesRetrieve     int
	// RatePerSec is the median of the payload's positive plant-wide
	// consumption rates (UoP per second) in the window, from the threshold
	// monitor's own tte_samples (kind threshold); 0 when none was measured.
	// Not the latest: a sample's rate is a sliding window's count, so after a
	// stop it decays to one unit per window before it reads 0, and the latest
	// positive sample of an idle part is that tail, not the line's rate.
	RatePerSec float64
}

// LeadTimesByPayload is the calculator's input read for every payload at once:
// one statement, one window, the same transition pairs, central tendencies and
// "0 = no signal" contract as AvgL1QueueSeconds, AvgL1TransitSeconds,
// MedianL2LoadSeconds, P95MarketToCellSeconds and the two
// CountCompletedOrdersInWindow coverage counts, grouped by payload instead of
// filtered to one (pinned against them). It also returns each payload's median
// positive rate from tte_samples, reached per monitored binding through
// idx_tte_samples_place_time, so the /inventory page can turn the rate into a
// cycle without a statement per row. A payload absent from the map had no
// signal in the window.
func LeadTimesByPayload(db *sql.DB, r LeadTimeRange) (map[string]LeadTimeInputs, error) {
	var args []any
	var ctes []string
	for _, t := range []struct {
		name         string
		from         []string
		fromEarliest bool
		to           string
		orderType    string
	}{
		{"l1q", domain.AcquiringEntryStatuses(), true, "dispatched", "retrieve_empty"},
		{"l1t", []string{"in_transit"}, false, "delivered", "retrieve_empty"},
		{"l2l", []string{"delivered"}, false, "confirmed", "retrieve_empty"},
		{"m2c", []string{"in_transit"}, false, "delivered", "retrieve"},
	} {
		var cte string
		cte, args = transitionsSQL(t.name, t.from, t.fromEarliest, t.to, OrderScope{OrderType: t.orderType}, r, true, args)
		ctes = append(ctes, cte)
	}
	args = append(args, r.Start.UTC(), r.End.UTC())
	start, end := len(args)-1, len(args)
	span := `EXTRACT(EPOCH FROM (to_ts - from_ts))`
	count := func(status, orderType string) string {
		return fmt.Sprintf(`SELECT o.payload_code, COUNT(DISTINCT o.id)::float8
			FROM orders o JOIN order_history h ON h.order_id = o.id
			WHERE h.status = '%s' AND h.created_at >= $%d AND h.created_at <= $%d AND o.order_type = '%s'
			GROUP BY o.payload_code`, status, start, end, orderType)
	}
	q := `WITH ` + strings.Join(ctes, ",\n") + `
		SELECT k, payload_code, v FROM (
			SELECT 'l1q' AS k, payload_code, AVG(` + span + `) AS v FROM l1q WHERE to_ts > from_ts GROUP BY payload_code
			UNION ALL
			SELECT 'l1t', payload_code, AVG(` + span + `) FROM l1t WHERE to_ts > from_ts GROUP BY payload_code
			UNION ALL
			SELECT 'l2l', payload_code, PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY ` + span + `) FROM l2l WHERE to_ts > from_ts GROUP BY payload_code
			UNION ALL
			SELECT 'm2c', payload_code, PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY ` + span + `) FROM m2c WHERE to_ts > from_ts GROUP BY payload_code
			UNION ALL
			SELECT 'n_l1', * FROM (` + count("confirmed", "retrieve_empty") + `) c1
			UNION ALL
			SELECT 'n_ret', * FROM (` + count("delivered", "retrieve") + `) c2
			UNION ALL
			SELECT 'rate', b.payload_code, PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY s.rate_per_sec)
			FROM (SELECT DISTINCT core_node_name, payload_code FROM demand_registry WHERE replenish_uop_threshold > 0) b
			JOIN tte_samples s ON s.core_node_name = b.core_node_name AND s.payload_code = b.payload_code
			 AND s.kind = 'threshold' AND s.rate_per_sec > 0 AND s.computed_at >= $` + fmt.Sprint(start) + `
			GROUP BY b.payload_code
		) u`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("lead times by payload: %w", err)
	}
	defer rows.Close()
	out := map[string]LeadTimeInputs{}
	for rows.Next() {
		var k, payload string
		var v sql.NullFloat64
		if err := rows.Scan(&k, &payload, &v); err != nil {
			return nil, fmt.Errorf("lead times by payload: scan: %w", err)
		}
		in := out[payload]
		switch k {
		case "l1q":
			in.L1QueueSeconds = v.Float64
		case "l1t":
			in.L1TransitSeconds = v.Float64
		case "l2l":
			in.L2LoadSeconds = v.Float64
		case "m2c":
			in.MarketToCellSeconds = v.Float64
		case "n_l1":
			in.SamplesL1 = int(v.Float64)
		case "n_ret":
			in.SamplesRetrieve = int(v.Float64)
		case "rate":
			in.RatePerSec = v.Float64
		}
		out[payload] = in
	}
	return out, rows.Err()
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
