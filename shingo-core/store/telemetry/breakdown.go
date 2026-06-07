package telemetry

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
)

// BreakdownRow is one grouped slice of missions for the §3.F breakdown panels
// (by robot, by route).
type BreakdownRow = domain.TelemetryBreakdownRow

// GetBreakdown returns the top-10 mission groups by count for the given
// dimension over the filter window (plan §3.F). by is "robot" (group by
// robot_id) or "route" (group by source→delivery). The group expression is
// chosen from a fixed switch — never interpolated from user input.
func GetBreakdown(db *sql.DB, f Filter, by string) ([]BreakdownRow, error) {
	var groupExpr string
	switch by {
	case "route":
		groupExpr = "COALESCE(NULLIF(source_node,''),'?') || ' → ' || COALESCE(NULLIF(delivery_node,''),'?')"
	default: // "robot"
		groupExpr = "robot_id"
	}
	where, args := buildWhere(f)
	if by != "route" {
		// Skip rows with no robot attributed.
		if where == "" {
			where = " WHERE robot_id <> ''"
		} else {
			where += " AND robot_id <> ''"
		}
	}
	q := fmt.Sprintf(`SELECT %s AS label, COUNT(*), COALESCE(AVG(duration_ms), 0)::BIGINT
		FROM mission_telemetry%s
		GROUP BY label ORDER BY COUNT(*) DESC LIMIT 10`, groupExpr, where)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BreakdownRow
	for rows.Next() {
		var r BreakdownRow
		if err := rows.Scan(&r.Label, &r.Count, &r.AvgDurationMS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
