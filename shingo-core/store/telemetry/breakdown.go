package telemetry

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
)

// BreakdownRow is one grouped slice of missions for the §3.F breakdown panels
// (by robot, by route).
type BreakdownRow = domain.TelemetryBreakdownRow

// RouteIndex is one robot's route index and the sample count behind it.
type RouteIndex = domain.TelemetryRouteIndex

// routeExpr is the route label, and it is deliberately ONE expression used by
// both the by-route grouping and the route-index denominator. Two copies of it
// would be two definitions of "the same route", and the index divides one by a
// median taken over the other — a drift between them would not fail, it would
// quietly index each mission against a different route's median.
const routeExpr = `COALESCE(NULLIF(source_node,''),'?') || ' → ' || COALESCE(NULLIF(delivery_node,''),'?')`

// GetBreakdown returns the top-10 mission groups by count for the given
// dimension over the filter window (plan §3.F). by is "robot" (group by
// robot_id) or "route" (group by source→delivery). The group expression is
// chosen from a fixed switch — never interpolated from user input.
func GetBreakdown(db *sql.DB, f Filter, by string) ([]BreakdownRow, error) {
	var groupExpr string
	switch by {
	case "route":
		groupExpr = routeExpr
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
	// FILTER (WHERE duration_ms > 0) matches every other duration query in
	// this package (see GetStats / GetStatsV2). Without it this was the one
	// consumer averaging non-positive durations straight into the bar list:
	// a clock skew that writes a negative duration_ms — which the sim does
	// on nearly every row — dragged the average to nonsense like -3.6e9 ms
	// while the same data read 46 s through the guarded queries. Count stays
	// unfiltered so the bar still reflects real volume.
	q := fmt.Sprintf(`SELECT %s AS label, COUNT(*),
		COALESCE(AVG(duration_ms) FILTER (WHERE duration_ms > 0), 0)::BIGINT
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

// GetRobotRouteIndex computes the U3 route index per robot, plus how many routes
// qualified to be a denominator at all.
//
// THE FIGURE, and why it is not the average it replaces. The panel used to show
// each robot's mean mission duration. Five reviewers wanted that retired outright
// and they were right about the reason: RDS assigns the routes, so the mean
// measures which routes a robot happened to draw. A robot parked on supermarket
// hauls reads slow and is not, and the panel then reliably concludes "AMR-04 is
// slow" about the one thing it cannot see. Indexing each mission against ITS OWN
// route's median removes the route mix from the comparison, so the number becomes
// a sentence about the robot: "this robot runs its routes 1.3x longer than they
// normally take."
//
// MEDIAN OF RATIOS, not ratio of means. Mission durations here are heavy-tailed
// (the guide says so and cycle time 5.10 measured it), and a mean of ratios lets
// one blocked trip decide a robot's number. The median of ratios asks the honest
// question — on a typical trip, is this robot slower than the route is.
//
// minRouteSamples EXCLUDES rather than greys. A route's median is the
// denominator, so a route with three missions supplies a "median" that is one of
// those three durations and the ratio against it is partly self-referential.
// Greying that would still print it. Excluding those missions means a robot's
// index is computed only where a real median existed — and the returned
// qualifyingRoutes is how the caller learns whether ANY route cleared the floor,
// which is when the column should be dropped from the table entirely rather than
// shown empty.
//
// duration_ms > 0 throughout, matching every other duration query in this
// package. The sim writes negative durations on nearly every row (clock skew),
// and a negative numerator would produce a negative index — a robot that reads
// as faster than instantaneous.
func GetRobotRouteIndex(db *sql.DB, f Filter, minRouteSamples int) (map[string]RouteIndex, int, error) {
	if minRouteSamples < 1 {
		// A floor below 1 is not a lenient setting, it is a broken one: n=1 makes
		// every mission its own median and every index exactly 1.0, which is a
		// column of 1.0s that looks like a finding. Config already falls back on
		// a non-positive value; this is the belt.
		minRouteSamples = 1
	}
	where, args := buildWhere(f)
	if where == "" {
		where = " WHERE robot_id <> '' AND duration_ms > 0"
	} else {
		where += " AND robot_id <> '' AND duration_ms > 0"
	}
	args = append(args, minRouteSamples)
	minArg := len(args)

	q := fmt.Sprintf(`
WITH m AS (
	SELECT robot_id, %s AS route, duration_ms
	FROM mission_telemetry%s
), r AS (
	SELECT route, COUNT(*) AS n,
	       percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) AS med
	FROM m GROUP BY route
), q AS (
	SELECT route, med FROM r WHERE n >= $%d AND med > 0
), idx AS (
	SELECT m.robot_id, m.duration_ms::float8 / q.med AS ratio
	FROM m JOIN q ON q.route = m.route
)
SELECT robot_id, COUNT(*) AS n,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY ratio) AS idx,
       (SELECT COUNT(*) FROM q) AS qualifying_routes
FROM idx GROUP BY robot_id`, routeExpr, where, minArg)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("robot route index: %w", err)
	}
	defer rows.Close()

	out := map[string]RouteIndex{}
	qualifying := 0
	for rows.Next() {
		var robot string
		var n int64
		var idx float64
		var qr int
		if err := rows.Scan(&robot, &n, &idx, &qr); err != nil {
			return nil, 0, fmt.Errorf("scan robot route index: %w", err)
		}
		qualifying = qr
		out[robot] = RouteIndex{Index: idx, Samples: n}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("robot route index rows: %w", err)
	}

	// Zero rows means no robot had a mission on a qualifying route — which does
	// NOT tell us whether any route qualified, because the correlated subquery
	// only rode along on rows that existed. Ask separately rather than reporting
	// "no routes qualified" from an absence of robots: the two states get
	// different UI (drop the column vs. an empty column), and inferring one from
	// the other is the reachability defect this repo already shipped once.
	if len(out) == 0 {
		if err := db.QueryRow(fmt.Sprintf(`
WITH m AS (SELECT %s AS route, duration_ms FROM mission_telemetry%s)
SELECT COUNT(*) FROM (
	SELECT route FROM m GROUP BY route
	HAVING COUNT(*) >= $%d AND percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) > 0
) x`, routeExpr, where, minArg), args...).Scan(&qualifying); err != nil {
			return nil, 0, fmt.Errorf("qualifying route count: %w", err)
		}
	}
	return out, qualifying, nil
}
