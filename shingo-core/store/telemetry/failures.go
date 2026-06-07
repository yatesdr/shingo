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
// reasons for the §3.G failure Pareto. It pulls the failed rows (mission
// counts are low-thousands/month, so this is fine) and classifies each in Go
// via domain.PrimaryFailureReason, returning the top-10 reasons by count with
// up to 5 sample order IDs each. Only terminal_state FAILED is folded in;
// operator cancels are not failures, and system-stop reclassification (which
// needs the order_history join) is left to a follow-up (Q-013).
func GetFailures(db *sql.DB, f Filter) ([]FailureReason, error) {
	where, args := buildWhere(f)
	cond := "terminal_state IN ('FAILED','failed')"
	if where == "" {
		where = " WHERE " + cond
	} else {
		where += " AND " + cond
	}
	// robot_alarms_json (Q-026) is the priority signal; it's NULL until the
	// alarm-snapshot ingestion lands (write side), in which case the classifier
	// falls through to blocks/errors. ::text feeds the string-based classifier.
	q := fmt.Sprintf(`SELECT order_id, COALESCE(robot_alarms_json::text,''), COALESCE(blocks_json,''), COALESCE(errors_json,'')
		FROM mission_telemetry%s`, where)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int64{}
	samples := map[string][]int64{}
	for rows.Next() {
		var orderID int64
		var robotAlarms, blocks, errors string
		if err := rows.Scan(&orderID, &robotAlarms, &blocks, &errors); err != nil {
			return nil, err
		}
		reason := domain.PrimaryFailureReason(robotAlarms, blocks, errors)
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
