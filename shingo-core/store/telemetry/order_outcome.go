package telemetry

import (
	"database/sql"
	"fmt"
	"strings"

	"shingocore/domain"
)

// Orders-table outcome source — the v2 dashboard success/cancel counts.
//
// The v2 success rate is an ORDER-FULFILLMENT metric, so its counts come from
// the orders table (the complete terminal record), NOT from mission_telemetry.
// mission_telemetry only rolls up a row when an order reaches a *vendor*
// terminal state (FINISHED/STOPPED — see fleet.IsTerminalState), so every
// failure that dies in Core's own pipeline before/without a vendor terminal —
// empty payload_code, no source bin, RDS SetOrder POST failure, reshuffle
// planning failure, grace-timeout abandon — never becomes a mission and is
// invisible to mission_telemetry. On a live plant that's the bulk of real
// failures. orders.status records them as 'failed'.
//
// orders.status is authoritative: confirmed / failed / cancelled / skipped are
// the terminal values ('delivered' is non-terminal — awaiting operator
// confirm, so it's excluded). success_rate = confirmed / (confirmed + failed);
// cancelled + skipped are excluded from the denominator (a deliberate cancel or
// a never-needed skip is not a fulfillment failure). Unlike the old
// mission_telemetry path there is NO stop reclassification — the orders
// pipeline already files grace/abandon/structural terminations as 'failed', so
// they count directly. Durations (execution / lead / P50 / P95) still come from
// mission_telemetry: only robot missions have a meaningful execution interval.

// orderTerminalStatusSQL is the terminal-status set for outcome counts.
// 'canceled' (US spelling) is included defensively though prod uses 'cancelled'.
const orderTerminalStatusSQL = `'confirmed','failed','cancelled','canceled','skipped'`

// orderOutcomeWhere builds the WHERE clause for orders-table outcome queries.
// It windows on COALESCE(completed_at, updated_at): confirmed orders set
// completed_at, but pre-dispatch failures/cancels never do — updated_at (the
// terminal-transition time) is the correct fallback. alias is the orders table
// alias ("" for an unaliased FROM orders).
func orderOutcomeWhere(alias string, f Filter) (string, []any) {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	conds := []string{p + "status IN (" + orderTerminalStatusSQL + ")"}
	var args []any
	add := func(expr string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(expr, len(args)))
	}
	if f.StationID != "" {
		add(p+"station_id=$%d", f.StationID)
	}
	if f.RobotID != "" {
		add(p+"robot_id=$%d", f.RobotID)
	}
	if f.Since != nil {
		add("COALESCE("+p+"completed_at, "+p+"updated_at) >= $%d", *f.Since)
	}
	if f.Until != nil {
		add("COALESCE("+p+"completed_at, "+p+"updated_at) <= $%d", *f.Until)
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// orderOutcomeCounts is the terminal-order count breakdown from the orders
// table. systemStops are already folded into failed by the pipeline.
type orderOutcomeCounts struct {
	total, confirmed, failed, cancelled, skipped int64
}

// getOrderOutcomeCounts returns terminal-order counts by outcome over the
// filter window from the orders table (the complete fulfillment picture).
func getOrderOutcomeCounts(db *sql.DB, f Filter) (orderOutcomeCounts, error) {
	where, args := orderOutcomeWhere("", f)
	q := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE status='confirmed'),
		COUNT(*) FILTER (WHERE status='failed'),
		COUNT(*) FILTER (WHERE status IN ('cancelled','canceled')),
		COUNT(*) FILTER (WHERE status='skipped')
		FROM orders` + where
	var c orderOutcomeCounts
	err := db.QueryRow(q, args...).Scan(&c.total, &c.confirmed, &c.failed, &c.cancelled, &c.skipped)
	return c, err
}

// cancelOrigins is the Q-030 origin split of the cancelled bucket.
type cancelOrigins struct {
	shingo, rds, unclassified int64
}

// getCancelOrigins splits cancelled orders by origin (Q-030) for the Cancelled
// tile sub-stat, sourcing from the orders table joined to each order's terminal
// order_history detail. Mirrors the retired classifyStops, but over orders and
// for the cancel set only — failures are no longer mixed into this query.
func getCancelOrigins(db *sql.DB, f Filter) (cancelOrigins, error) {
	where, args := orderOutcomeWhere("o", f)
	where += " AND o.status IN ('cancelled','canceled')"
	q := `SELECT COALESCE(oh.detail, '')
		FROM orders o
		LEFT JOIN LATERAL (
			SELECT detail FROM order_history h
			WHERE h.order_id = o.id
			ORDER BY h.created_at DESC, h.id DESC
			LIMIT 1
		) oh ON TRUE` + where
	rows, err := db.Query(q, args...)
	if err != nil {
		return cancelOrigins{}, err
	}
	defer rows.Close()
	var o cancelOrigins
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return cancelOrigins{}, err
		}
		switch domain.ClassifyCancelOrigin(detail) {
		case domain.CancelOriginShingo:
			o.shingo++
		case domain.CancelOriginRDS:
			o.rds++
		default:
			o.unclassified++
		}
	}
	return o, rows.Err()
}
