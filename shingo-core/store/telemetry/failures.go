package telemetry

import (
	"database/sql"
	"fmt"
	"sort"

	"shingocore/domain"
)

// FailureReason is the §3.G Pareto row type.
type FailureReason = domain.FailureReason

// GetFailures classifies failed missions in the window into categorical
// reasons for the §3.G failure Pareto. It folds in BOTH hard FAILED missions
// and system-initiated stops — STOPPED/cancelled missions whose terminal
// order_history detail classifies as a failure (grace timeouts, abandons,
// structural faults) per domain.ClassifyTermination (Q-013). Operator and RDS
// cancels stay excluded. Each row is classified in Go via
// domain.PrimaryFailureReason (robot_alarms → errors → blocks); when those give
// nothing — the usual system-stop case — the reason is derived from the
// terminal detail via domain.SystemStopReason. Returns the top-10 reasons by
// count with up to 5 sample order IDs each. Mission counts are
// low-thousands/month, so pulling + classifying in Go is fine.
func GetFailures(db *sql.DB, f Filter) ([]FailureReason, error) {
	where, args := buildWhere(f)
	// Pull hard failures AND the stop/cancel bucket; the latter is reclassified
	// in Go so only the system-stop subset counts as a failure.
	cond := "terminal_state IN ('FAILED','failed','STOPPED','stopped','cancelled','canceled')"
	if where == "" {
		where = " WHERE " + cond
	} else {
		where += " AND " + cond
	}
	// LEFT JOIN LATERAL → the terminal order_history detail (one row per
	// mission, '' when no history), the same shape classifyStops uses. The three
	// signal columns (robot_alarms_json is the Q-026 priority source, NULL until
	// the alarm-snapshot ingestion lands) feed PrimaryFailureReason. All are
	// JSONB; ::text before COALESCE-against-'' dodges the 22P02 that 500'd
	// /api/missions/failures historically.
	q := fmt.Sprintf(`SELECT mt.order_id, mt.terminal_state,
			COALESCE(mt.robot_alarms_json::text,''), COALESCE(mt.blocks_json::text,''), COALESCE(mt.errors_json::text,''),
			COALESCE(oh.detail,'')
		FROM mission_telemetry mt
		LEFT JOIN LATERAL (
			SELECT detail FROM order_history oh
			WHERE oh.order_id = mt.order_id
			ORDER BY oh.created_at DESC, oh.id DESC
			LIMIT 1
		) oh ON TRUE%s`, where)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int64{}
	samples := map[string][]int64{}
	for rows.Next() {
		var orderID int64
		var ts, robotAlarms, blocks, errors, detail string
		if err := rows.Scan(&orderID, &ts, &robotAlarms, &blocks, &errors, &detail); err != nil {
			return nil, err
		}
		// Hard FAILED always counts; a stop/cancel counts only when
		// ClassifyTermination places it in the failed bucket (grace/timeout/
		// structural/abandon). Operator + RDS cancels fall out here.
		if domain.ClassifyTermination(ts, detail) != domain.OutcomeFailed {
			continue
		}
		reason := domain.PrimaryFailureReason(robotAlarms, blocks, errors)
		if reason == domain.FailOther {
			if d := domain.SystemStopReason(detail); d != "" {
				reason = d
			}
		}
		counts[reason]++
		if len(samples[reason]) < 5 {
			samples[reason] = append(samples[reason], orderID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]FailureReason, 0, len(counts))
	for reason, c := range counts {
		out = append(out, FailureReason{Reason: reason, Count: c, SampleOrderIDs: samples[reason]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason // stable tie-break
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out, nil
}
