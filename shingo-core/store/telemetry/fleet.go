package telemetry

import (
	"database/sql"
	"fmt"
	"time"
)

// RobotMissionAgg is per-robot mission activity over a window — the basis for
// the v1 utilization bar (% of the window a robot spent executing missions)
// and the per-robot rows on the Robot Fleet section (plan §15.C).
type RobotMissionAgg struct {
	RobotID  string `json:"robot_id"`
	Missions int64  `json:"missions"`
	BusyMS   int64  `json:"busy_ms"`
}

// GetRobotMissionAggs returns mission count + summed duration per robot over
// the filter window (robot_id scoping in the filter is ignored here — the
// fleet view wants every robot). Mirrors GetStats' WHERE via buildWhere.
func GetRobotMissionAggs(db *sql.DB, f Filter) ([]RobotMissionAgg, error) {
	// Drop any robot_id filter — the fleet view aggregates the whole fleet.
	f.RobotID = ""
	where, args := buildWhere(f)
	cond := "robot_id <> ''"
	if where == "" {
		where = " WHERE " + cond
	} else {
		where += " AND " + cond
	}
	// busy_ms is execution time (assignment→terminal), not lead time — the robot
	// wasn't busy while the order sat in a queue (Q-031). SUM skips missions that
	// never reached a robot (NULL execution).
	q := fmt.Sprintf(`SELECT robot_id, COUNT(*), COALESCE(SUM(%s), 0)::BIGINT
		FROM mission_telemetry mt%s GROUP BY robot_id`, executionMSExpr("mt"), where)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RobotMissionAgg
	for rows.Next() {
		var a RobotMissionAgg
		if err := rows.Scan(&a.RobotID, &a.Missions, &a.BusyMS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// HourConcurrency is one hour's fleet concurrency: how many distinct robots
// had a mission overlapping that hour. Powers the Fleet Load chart's
// "robots concurrently used" curve (plan §15.C).
type HourConcurrency struct {
	Hour        time.Time `json:"hour"`
	Concurrency int64     `json:"concurrency"`
}

// GetHourlyConcurrency returns 24 hourly concurrency points for the calendar
// day starting at dayStart. A robot counts toward an hour if its *execution*
// interval [assignment, completion] overlaps that hour — execution time
// (Q-031), so a robot isn't counted busy while its order merely queued or after
// it delivered (awaiting confirm). Both endpoints come from order_history (same
// clock). The CTE computes them once per mission. An optional stationID scopes
// to one station's missions ("" = all).
func GetHourlyConcurrency(db *sql.DB, dayStart time.Time, stationID string) ([]HourConcurrency, error) {
	args := []any{dayStart}
	stationCond := ""
	if stationID != "" {
		stationCond = " AND exec.station_id = $2"
		args = append(args, stationID)
	}
	q := fmt.Sprintf(`WITH exec AS (
			SELECT mt.robot_id, mt.station_id, %s AS started, %s AS ended
			FROM mission_telemetry mt
			WHERE mt.robot_id <> ''
		)
		SELECT gs AS hour, COUNT(DISTINCT exec.robot_id) AS concurrency
		FROM generate_series($1::timestamptz, $1::timestamptz + interval '23 hours', interval '1 hour') gs
		LEFT JOIN exec
		  ON exec.started IS NOT NULL AND exec.ended IS NOT NULL
		  AND exec.started < gs + interval '1 hour'
		  AND exec.ended >= gs%s
		GROUP BY gs ORDER BY gs`, assignmentExpr("mt"), completionExpr("mt"), stationCond)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourConcurrency
	for rows.Next() {
		var h HourConcurrency
		if err := rows.Scan(&h.Hour, &h.Concurrency); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
