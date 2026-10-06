package telemetry

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
)

// queryNoJIT runs a query with JIT compilation disabled, handing the rows to
// scan before the transaction closes.
//
// The concurrency queries are cheap to EXECUTE and expensive to PLAN: at
// Springfield, 322ms of GetHourlyConcurrency's 526ms was JIT compilation. The
// planner's cost estimate crosses jit_above_cost because of the
// generate_series x correlated-subquery shape, then spends most of the budget
// compiling a query that touches a few thousand rows.
//
// SET LOCAL needs a transaction — a bare SET on a pooled connection leaks the
// setting to whatever runs next on it. The callback shape is what keeps the tx
// alive for exactly as long as the cursor and no longer; read-only, so the
// deferred rollback is the correct close in every path.
func queryNoJIT(db *sql.DB, query string, args []any, scan func(*sql.Rows) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // read-only; rollback is the close

	if _, err := tx.Exec(`SET LOCAL jit = off`); err != nil {
		// Not fatal: a Postgres built without JIT rejects this. Run the query
		// anyway rather than failing the panel over a planner hint.
		log.Printf("telemetry: SET LOCAL jit=off unavailable, running with JIT: %v", err)
	}

	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scan(rows)
}

// ── Busy time: finished missions AND open orders ───────────────────────────
//
// A robot is busy from its assignment (the first acknowledged/in_transit row,
// see assignmentExpr) until it puts the load down. For a finished mission that
// end is completionExpr. For an order that is still open it is NOW: the robot
// is on it — moving, or held at a staging node with the bin on its deck — and
// that time is exactly as spent as a finished mission's. Counting only finished
// missions made a robot parked on a held order for nine hours read as the least
// used robot in the fleet.
//
// "Open" is: not terminal, a robot on it, and no completion row yet. The last
// clause keeps a delivered-awaiting-confirm order out — the robot already put
// it down, and its mission_telemetry row (written at the vendor terminal) has
// that span already. An open order that has a telemetry row but no completion
// (a vendor failure parked in `faulted`) contributes nothing from that row,
// because completionExpr is NULL for it, so nothing is counted twice.

// openOrderCond is the WHERE body selecting open orders a robot is on. alias is
// the orders table alias.
func openOrderCond(alias string) string {
	return fmt.Sprintf(`%[1]s.robot_id <> '' AND %[1]s.status NOT IN (%[2]s)
		AND NOT EXISTS (SELECT 1 FROM order_history oc
			WHERE oc.order_id = %[1]s.id AND oc.status IN (`+completionStatesSQL+`))`,
		alias, protocol.TerminalStatusSQLList())
}

// busyIntervalsCTE is the `exec` and `busy` CTEs every fleet query reads: the
// per-robot busy time, the hourly curve, the peak and the daily rollup.
// `exec(robot_id, s, e)` is one row per interval a robot spent on an order,
// finished missions and open orders together, restricted to intervals
// overlapping [startArg, endArg); an open order's interval ends at nowArg.
// `busy(robot_id, s, e)` is the same instants merged per robot (robotUnionCTEs),
// so a robot on two orders at once is one robot for that time, not two.
// stationCond, when non-empty, is appended to the filter (it reads the
// station_id column).
//
// The mission_telemetry half keeps its core_created/core_completed bound for
// the index: those bracket the computed execution interval, so it is a superset
// filter and cannot drop a mission that overlaps the window.
//
// ONE READ OF ORDER HISTORY. Each order's bounds (its first assignment row and
// its first completion row, the statuses assignmentExpr and completionExpr
// read) come from one grouped pass over the history of the orders in play,
// not two correlated subqueries per mission. Over 30 days at plant scale
// (7,500 missions) the correlated shape cost the per-robot query 1.3 s with
// JIT on and 0.2 s with it off.
func busyIntervalsCTE(startArg, endArg, nowArg, stationCond string) string {
	return fmt.Sprintf(`exec_ord AS (
			SELECT mt.robot_id, mt.station_id, mt.order_id, false AS open
			FROM mission_telemetry mt
			WHERE mt.robot_id <> ''
			  AND mt.core_completed >= %[1]s::timestamptz
			  AND mt.core_created <= %[2]s::timestamptz
			UNION ALL
			SELECT o.robot_id, o.station_id, o.id, true
			FROM orders o
			WHERE %[4]s
		),
		exec_b AS (
			SELECT oh.order_id,
			       MIN(oh.created_at) FILTER (WHERE oh.status IN (`+assignmentStatesSQL+`)) AS s,
			       MIN(oh.created_at) FILTER (WHERE oh.status IN (`+completionStatesSQL+`)) AS e
			FROM order_history oh
			WHERE oh.order_id IN (SELECT order_id FROM exec_ord)
			GROUP BY oh.order_id
		),
		exec AS (
			SELECT robot_id, started AS s, ended AS e FROM (
				SELECT x.robot_id, x.station_id, b.s AS started,
				       CASE WHEN x.open THEN %[3]s::timestamptz ELSE b.e END AS ended
				FROM exec_ord x JOIN exec_b b ON b.order_id = x.order_id
			) iv
			WHERE started IS NOT NULL AND ended IS NOT NULL AND started < ended
			  AND started < %[2]s::timestamptz AND ended > %[1]s::timestamptz%[5]s
		),
		%[6]s`,
		startArg, endArg, nowArg, openOrderCond("o"), stationCond, robotUnionCTEs("exec", "busy"))
}

// robotUnionCTEs merges src(robot_id, s, e) into name(robot_id, s, e): per
// robot, the disjoint intervals covering exactly the instants src covers.
//
// A ROBOT IS BUSY OR IT IS NOT. Nothing in Core keeps a robot on one open order
// at a time — an order whose terminal report was lost stays open while the
// robot runs its next mission — so summing intervals counted those hours twice,
// and the min(busy, window) cap in the handler only hid it at 100%. Pinned by
// TestOverviewPin_OverlapIsCountedOnce.
//
// Gaps and islands: sorted by start, an interval opens a new island when it
// starts after every earlier interval of that robot has ended. Touching
// intervals merge, which changes no length.
func robotUnionCTEs(src, name string) string {
	return fmt.Sprintf(`%[2]s_brk AS (
			SELECT robot_id, s, e,
			       CASE WHEN s <= MAX(e) OVER (PARTITION BY robot_id ORDER BY s, e
			                ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING)
			            THEN 0 ELSE 1 END AS brk
			FROM %[1]s
		),
		%[2]s_grp AS (
			SELECT robot_id, s, e,
			       SUM(brk) OVER (PARTITION BY robot_id ORDER BY s, e ROWS UNBOUNDED PRECEDING) AS g
			FROM %[2]s_brk
		),
		%[2]s AS (
			SELECT robot_id, MIN(s) AS s, MAX(e) AS e FROM %[2]s_grp GROUP BY robot_id, g
		)`, src, name)
}

// peakPerBucketCTEs adds two CTEs after `busy` and a `buckets(gs)` CTE the
// caller supplies: `sweep`, a running count of robots on an order, and
// `peaks(gs, peak)`, its maximum inside each hour bucket.
//
// ONE PASS OVER THE EVENTS. Every busy interval is +1 at its start and −1 at
// its end, and every bucket start is a 0 marker; one ordered window sum is the
// number of robots on an order after each event, and the latest marker at or
// before an event names its bucket. The count only rises at a start, so a
// bucket's peak is the larger of the count at its marker and the counts after
// the starts inside it. busy is disjoint per robot, so the count is robots, not
// orders. This replaced a join of every candidate instant against every
// interval, which grew with the square of the window's missions (30 days at
// plant scale: 7.3 s; the cost table is in the round-2 report).
//
// Ties at one instant sort ends, then the marker, then starts: intervals are
// half-open [s, e), so a mission that ends on the hour is not on the next one,
// and one that starts on the hour is. An end on a bucket boundary is assigned
// to the bucket before and filtered out by t < gs + 1 hour, as is anything
// after the last bucket; events before the first bucket only seed the count.
const peakPerBucketCTEs = `,
		ev AS (
			SELECT s AS t, 2 AS k, 1 AS d FROM busy
			UNION ALL SELECT e, 0, -1 FROM busy
			UNION ALL SELECT gs, 1, 0 FROM buckets
		),
		sweep AS (
			SELECT t, SUM(d) OVER w AS c, MAX(CASE WHEN k = 1 THEN t END) OVER w AS gs
			FROM ev WINDOW w AS (ORDER BY t, k ROWS UNBOUNDED PRECEDING)
		),
		peaks AS (
			SELECT gs, MAX(c)::BIGINT AS peak FROM sweep
			WHERE gs IS NOT NULL AND t < gs + interval '1 hour'
			GROUP BY gs
		)`

// RobotMissionAgg is per-robot mission activity over a window — the basis for
// the utilization bar (% of the window a robot spent on orders) and the
// per-robot rows on the Robot Fleet section (plan §15.C).
//
// Missions counts finished missions only; an open order is not a mission yet.
// BusyMS counts both — see busyIntervalsCTE.
type RobotMissionAgg struct {
	RobotID  string `json:"robot_id"`
	Missions int64  `json:"missions"`
	BusyMS   int64  `json:"busy_ms"`
}

// GetRobotMissionAggs returns mission count + busy time per robot over the
// filter window (robot_id scoping in the filter is ignored here — the fleet
// view wants every robot).
//
// ONE INTERVAL SET. Busy time reads busyIntervalsCTE, the intervals behind the
// hourly curve, the peak and the daily average: every finished mission or open
// order overlapping the window, assignment to completion (or to now), merged
// per robot (robotUnionCTEs) and clipped to [Since, min(now, Until)]. A mission
// that crosses a window edge counts only its part inside the window, as it
// does in the curve; selecting finished missions by core_completed and
// counting them whole put a mission that crossed the start in whole and one
// that crossed the end in not at all. FAULTED TIME IS BUSY (R9): a robot whose
// order is faulted is still on it and cannot take other work.
//
// Missions is the finished missions GetStats' WHERE selects (buildWhere):
// finished in the window, and in the State if one is given. State does not
// narrow busy time, which is the robot's, not an outcome's.
func GetRobotMissionAggs(db *sql.DB, f Filter, now time.Time) ([]RobotMissionAgg, error) {
	// Drop any robot_id filter — the fleet view aggregates the whole fleet.
	f.RobotID = ""
	where, args := buildWhere(f)
	cond := "robot_id <> ''"
	if where == "" {
		where = " WHERE " + cond
	} else {
		where += " AND " + cond
	}

	clipStart := time.Time{}
	if f.Since != nil {
		clipStart = *f.Since
	}
	clipEnd := now
	if f.Until != nil && f.Until.Before(clipEnd) {
		clipEnd = *f.Until
	}
	args = append(args, clipStart, clipEnd, now)
	n := len(args)
	startArg, endArg, nowArg := fmt.Sprintf("$%d", n-2), fmt.Sprintf("$%d", n-1), fmt.Sprintf("$%d", n)
	stationCond := ""
	if f.StationID != "" {
		args = append(args, f.StationID)
		stationCond = fmt.Sprintf(" AND station_id = $%d", len(args))
	}

	q := fmt.Sprintf(`WITH %[1]s,
		fin AS (
			SELECT mt.robot_id, COUNT(*) AS n FROM mission_telemetry mt%[2]s GROUP BY mt.robot_id
		)
		SELECT robot_id, SUM(n)::BIGINT, ROUND(SUM(busy))::BIGINT FROM (
			SELECT robot_id, n, 0::float8 AS busy FROM fin
			UNION ALL
			SELECT robot_id, 0, SUM(EXTRACT(EPOCH FROM (
				LEAST(e, %[4]s::timestamptz) - GREATEST(s, %[3]s::timestamptz))) * 1000)::float8
			FROM busy GROUP BY robot_id
		) a GROUP BY robot_id`,
		busyIntervalsCTE(startArg, endArg, nowArg, stationCond), where, startArg, endArg)

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

// HourConcurrency is one hour's fleet concurrency: the most robots on an order
// at the same instant within that hour (finished missions and open orders).
// Powers the Fleet Load chart's "robots used" curve (plan §15.C).
type HourConcurrency struct {
	Hour        time.Time `json:"hour"`
	Concurrency int64     `json:"concurrency"`
}

// GetHourlyConcurrency returns one concurrency point per hour of the plant day
// [dayStart, dayEnd): 24 on most days, 23 or 25 on a DST change, because the
// caller passes the next plant midnight rather than this assuming 24 hours.
// The series stops at the hour now falls in: an hour that has not happened is
// not a measured zero, and drawing it as one dived the curve to the floor at
// the right edge of every Today chart.
// Intervals are execution time (Q-031) — assignment to completion from
// order_history, so a robot isn't counted while its order merely queued or
// after it delivered — plus open orders up to now. An optional stationID
// scopes to one station's orders ("" = all).
func GetHourlyConcurrency(db *sql.DB, dayStart, dayEnd, now time.Time, stationID string) ([]HourConcurrency, error) {
	args := []any{dayStart, dayEnd, now}
	stationCond := ""
	if stationID != "" {
		stationCond = " AND station_id = $4"
		args = append(args, stationID)
	}
	// The exec CTE is bounded by the caller's window. Unbounded it ran the two
	// correlated order_history subqueries over EVERY mission ever recorded to
	// answer a question about 24 hours, and grew with the table forever.
	q := `WITH ` + busyIntervalsCTE("$1", "$2", "$3", stationCond) + `,
		buckets AS (
			SELECT gs FROM generate_series($1::timestamptz, LEAST($2::timestamptz - interval '1 hour', $3::timestamptz), interval '1 hour') gs
		)` + peakPerBucketCTEs + `
		SELECT gs AS hour, peak AS concurrency FROM peaks ORDER BY gs`
	var out []HourConcurrency
	err := queryNoJIT(db, q, args, func(rows *sql.Rows) error {
		for rows.Next() {
			var h HourConcurrency
			if err := rows.Scan(&h.Hour, &h.Concurrency); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DayConcurrency is one day's fleet-concurrency rollup for the multi-day Fleet
// Load view (7d/30d): the day's peak (most robots on an order at one instant)
// and its average (robot-time on orders ÷ the day's elapsed time — the mean
// number of robots in use). The single-day (Today) view keeps the hourly
// HourConcurrency curve.
type DayConcurrency struct {
	Day  time.Time `json:"day"`
	Peak int64     `json:"peak"`
	Avg  float64   `json:"avg"`
}

// GetDailyConcurrency rolls fleet concurrency up to per-day peak/avg over
// [since, until], so the Fleet Load chart honors the range selector instead of
// only ever showing one day. Same intervals as GetHourlyConcurrency, computed
// hourly across the whole range then grouped by day. The average is the
// robots' merged busy time (robotUnionCTEs) over the time that has ELAPSED
// (each hour up to now), so today's average is not diluted by hours that have
// not happened. Each merged interval is cut into the hours it spans by
// arithmetic from since, then joined to its bucket on equality — not compared
// against every bucket, which was intervals × hours.
//
// DAYS ARE PLANT DAYS. The hours step in absolute time from since (a plant
// midnight) and are grouped by their local date in tz, the plant zone passed as
// a bound parameter — the session is UTC, so a bare date_trunc('day') cut every
// day at UTC midnight: an evening mission at a Central plant landed on the next
// day, and a seven-day window came back as eight buckets with a partial edge at
// each end. Each day is labelled with its plant midnight; the fall-back day is
// 25 hours long and is one bucket. An optional stationID scopes to one
// station's orders ("" = all).
func GetDailyConcurrency(db *sql.DB, since, until, now time.Time, tz, stationID string) ([]DayConcurrency, error) {
	args := []any{since, until, now, tz}
	stationCond := ""
	if stationID != "" {
		stationCond = " AND station_id = $5"
		args = append(args, stationID)
	}
	// Same window bound as GetHourlyConcurrency — see the note there.
	q := `WITH ` + busyIntervalsCTE("$1", "($2::timestamptz + interval '1 hour')", "$3", stationCond) + `,
		buckets AS (
			SELECT gs FROM generate_series($1::timestamptz, $2::timestamptz, interval '1 hour') gs
		)` + peakPerBucketCTEs + `,
		busy_h AS (
			SELECT gs, SUM(EXTRACT(EPOCH FROM (
				LEAST(u.e, gs + interval '1 hour') - GREATEST(u.s, gs)))) AS busy_s
			FROM busy u CROSS JOIN LATERAL generate_series(
				$1::timestamptz + floor(EXTRACT(EPOCH FROM (GREATEST(u.s, $1::timestamptz) - $1::timestamptz)) / 3600) * interval '1 hour',
				LEAST(u.e, $2::timestamptz + interval '1 hour') - interval '1 microsecond',
				interval '1 hour') gs
			GROUP BY gs
		),
		hours AS (
			SELECT b.gs, COALESCE(h.busy_s, 0) AS busy_s,
				GREATEST(EXTRACT(EPOCH FROM (
					LEAST(b.gs + interval '1 hour', $3::timestamptz) - b.gs)), 0) AS elapsed_s
			FROM buckets b LEFT JOIN busy_h h ON h.gs = b.gs
		)
		SELECT (date_trunc('day', b.gs AT TIME ZONE $4::text) AT TIME ZONE $4::text) AS day,
			MAX(p.peak)::BIGINT AS peak,
			COALESCE(SUM(b.busy_s) / NULLIF(SUM(b.elapsed_s), 0), 0)::float8 AS avg
		FROM hours b JOIN peaks p ON p.gs = b.gs
		GROUP BY 1 ORDER BY 1`
	var out []DayConcurrency
	err := queryNoJIT(db, q, args, func(rows *sql.Rows) error {
		for rows.Next() {
			var d DayConcurrency
			if err := rows.Scan(&d.Day, &d.Peak, &d.Avg); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
