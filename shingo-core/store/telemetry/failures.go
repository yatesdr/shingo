package telemetry

import (
	"database/sql"
	"fmt"
	"sort"

	"shingo/protocol"
	"shingocore/domain"
)

// FailureReason is the §3.G Pareto row type.
type FailureReason = domain.FailureReason

// GetFailures classifies failed orders in the window into categorical reasons
// for the §3.G failure Pareto.
//
// Reads ORDERS, not mission_telemetry. mission_telemetry only gets a row when
// an order reaches a VENDOR terminal, so 76.6% of terminal orders have no row
// at all — every skip, most cancels, and every failure that dies in Core's own
// pipeline (empty payload_code, no source bin, RDS POST failure, reshuffle
// planning failure, grace-timeout abandon) never becomes a mission. On a live
// plant that is the bulk of real failures, and this page was blind to all of
// them while the Overview tile beside it counted from orders. Two populations,
// two pages, nothing reconciling them.
//
// EXPECT THE COUNT TO JUMP. That is the fix, not a regression.
//
// mission_telemetry is still LEFT JOINed for the vendor signal — robot alarms,
// block and error payloads — because that is genuinely what it holds. An order
// with no mission row simply classifies from its terminal detail instead.
//
// Same windowing and filters as the v2 outcome counts (orderOutcomeWhere), so
// the Pareto and the success-rate tile finally describe one population.
// Returns the top-10 reasons by count with up to 5 sample order IDs each.
func GetFailures(db *sql.DB, f Filter) ([]FailureReason, error) {
	where, args := orderOutcomeWhere("o", f)
	// orderOutcomeWhere already restricts to terminal statuses; narrow to the
	// ones that can classify as a failure. 'confirmed' never can, and neither
	// can 'skipped' — ClassifyTermination buckets it as skipped regardless of
	// detail. The cancels are pulled because only a SUBSET of them are
	// failures (grace / timeout / structural / abandon), decided in Go below;
	// operator and RDS cancels drop out there.
	where += " AND o.status IN ('failed','cancelled','canceled')"

	q := fmt.Sprintf(`SELECT o.id, o.status,
			COALESCE(mt.robot_alarms_json::text,''), COALESCE(mt.blocks_json::text,''), COALESCE(mt.errors_json::text,''),
			COALESCE(oh.detail,''), COALESCE(oh.code,'')
		FROM orders o
		LEFT JOIN mission_telemetry mt ON mt.order_id = o.id
		LEFT JOIN LATERAL (
			SELECT detail, code FROM order_history oh
			WHERE oh.order_id = o.id
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
		var ts, robotAlarms, blocks, errors, detail, code string
		if err := rows.Scan(&orderID, &ts, &robotAlarms, &blocks, &errors, &detail, &code); err != nil {
			return nil, err
		}
		// Prefer the TYPED code (migration 55). Fall back to reading the prose
		// detail only for rows written before the column existed — that
		// substring matching is the bug class that once classified 100% of
		// failures as "Robot blocked", so it is a compatibility shim, not the
		// mechanism.
		outcome, typed := domain.ClassifyTermCode(protocol.Status(ts), protocol.TermCode(code))
		if !typed {
			outcome = domain.ClassifyTermination(ts, detail)
		}
		if outcome != domain.OutcomeFailed {
			continue
		}
		// The vendor signal wins when there is one — a robot alarm names the
		// physical fault, which a code cannot.
		reason := domain.PrimaryFailureReason(robotAlarms, blocks, errors)
		if reason == domain.FailOther && code != "" {
			reason = code
		}
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
