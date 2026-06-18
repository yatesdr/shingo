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

// DayConcurrency is one day's fleet-concurrency rollup for the multi-day Fleet
// Load view (7d/30d): the day's peak (max simultaneous robots in any hour) and
// its average hourly concurrency. The single-day (Today) view keeps the hourly
// HourConcurrency curve.
type DayConcurrency struct {
	Day  time.Time `json:"day"`
	Peak int64     `json:"peak"`
	Avg  float64   `json:"avg"`
}

// GetDailyConcurrency rolls hourly fleet concurrency up to per-day peak/avg over
// [since, until], so the Fleet Load chart honors the range selector instead of
// only ever showing one day. Same overlap definition as GetHourlyConcurrency (a
// robot counts toward an hour if its execution interval overlaps it), computed
// hourly across the whole range then grouped by day. Hourly across 30 days is
// cheap to COMPUTE (~720 rows); it's only unreadable to CHART, which is why the
// multi-day view shows daily peak/avg rather than 720 hourly points. Days are
// truncated in UTC to match the daily trend buckets on the same page. An
// optional stationID scopes to one station's missions ("" = all).
func GetDailyConcurrency(db *sql.DB, since, until time.Time, stationID string) ([]DayConcurrency, error) {
	args := []any{since, until}
	stationCond := ""
	if stationID != "" {
		stationCond = " AND exec.station_id = $3"
		args = append(args, stationID)
	}
	q := fmt.Sprintf(`WITH exec AS (
			SELECT mt.robot_id, mt.station_id, %s AS started, %s AS ended
			FROM mission_telemetry mt
			WHERE mt.robot_id <> ''
		),
		hourly AS (
			SELECT gs AS hour, COUNT(DISTINCT exec.robot_id) AS concurrency
			FROM generate_series($1::timestamptz, $2::timestamptz, interval '1 hour') gs
			LEFT JOIN exec
			  ON exec.started IS NOT NULL AND exec.ended IS NOT NULL
			  AND exec.started < gs + interval '1 hour'
			  AND exec.ended >= gs%s
			GROUP BY gs
		)
		SELECT date_trunc('day', hour) AS day,
			MAX(concurrency)::BIGINT AS peak,
			AVG(concurrency)::float8 AS avg
		FROM hourly
		GROUP BY 1 ORDER BY 1`, assignmentExpr("mt"), completionExpr("mt"), stationCond)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayConcurrency
	for rows.Next() {
		var d DayConcurrency
		if err := rows.Scan(&d.Day, &d.Peak, &d.Avg); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
