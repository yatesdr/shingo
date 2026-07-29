package counters

// daily.go — the permanent end of the counting ladder, and the 90-day
// retention on the rung above it.
//
//	counter_snapshots  raw, one row per poll   14 days   retention.go
//	hourly_counts      per process/style/hour  90 days   this file
//	daily_counts       per process/style/day   permanent this file
//
// THE ROLLUP AND THE HOURLY PURGE LIVE IN ONE FILE ON PURPOSE. The purge's
// predicate is defined by the rollup — it deletes an hour only when the day it
// belongs to has already been summarised — and that coupling is the entire
// safety argument. Split across two files it is a convention, and the first
// edit that changes one without the other silently turns "aggregate, then
// delete" into "delete".

import (
	"database/sql"
	"fmt"
	"time"

	"shingoedge/domain"
)

// DailyCount is the daily_counts row type. The struct lives in
// shingoedge/domain; this alias matches the Snapshot / HourlyCount /
// ReportingPoint pattern at the top of counters.go.
type DailyCount = domain.DailyCount

// DateLayout is the shape of hourly_counts.count_date and
// daily_counts.count_date.
//
// IT IS A PLANT-LOCAL DATE, NOT A UTC ONE. engine.HourlyTracker.HandleDelta
// formats it from time.Now().In(ht.loc), where loc comes from cfg.Timezone
// (engine.go, NewHourlyTracker). So every cutoff computed against these
// columns has to be rendered in the SAME location, which is why the functions
// below take a *time.Location instead of reaching for time.Now().UTC(). At
// Springfield (UTC-5) a UTC-derived cutoff is one calendar day too aggressive
// for five hours out of every twenty-four — small, invisible, and free to
// avoid.
const DateLayout = "2006-01-02"

// HourlyRetention is how long the Edge keeps per-hour detail.
//
// NINETY DAYS BECAUSE THE DAY'S TOTAL IS ALREADY SOMEWHERE ELSE, not because
// of size. Measured on the restored Springfield database of 2026-07-27
// (edge-golden.db, md5 22fd294c2aa0b62d278c636e774f6a4a for its edge.sql):
// hourly_counts is 2,907 rows over 70 distinct dates, 143,360 B of table plus
// 86,016 B of unique index. That is 229,376 B — 1.14% of the restored file's
// 20,164,608 B, or 0.7% of the 31.60 MB the LIVE Pi carries, the difference
// being the 11.33 MB of freelist a restore does not reproduce. Both denominators
// are stated because quoting one against the other is how a size argument
// quietly becomes wrong. A 90-day window deletes 78 of those rows today. Nobody
// would run this pass for the bytes.
//
// The reason to run it is that hourly detail is a shift-shaped read and the
// day total is the one that survives the question. www/handlers_production.go
// takes ONE date at a time and renders it as an hour-by-hour chart against the
// shift boundaries; daily_counts holds the same production in 11.7x fewer rows
// — 248 against 2,907 on identical data, measured.
//
// STATE THE COST PLAINLY: ?date= on the production page is free text, so an
// engineer CAN ask for a date older than this window, and after this deploys
// that chart comes back empty where today it comes back populated. The day's
// total is still there, via ListDaily and GET /api/daily-counts. That is the
// trade, and it is the trade the ladder is: hour-level resolution for a
// quarter, day-level for good.
//
// The window matters more later than now. counters.SnapshotRetention's comment
// works the cell expansion out for raw snapshots; the same six-to-forty counter
// expansion takes hourly_counts from the 94.3 rows per producing day measured
// across July (1,132 rows over 12 dates) to roughly 630, and 90 days is what
// keeps that flat instead of accumulating for the life of the box.
//
// It also closes a specific FK repair: RUNBOOK-0.5's ordered data plan leaves
// exactly one dangling row behind, hourly_counts id 144030 (style 32,
// count_date 2026-06-25), and this window is what removes it — on 2026-09-23,
// ninety days after that date, NOT on the deploy. Anyone timing the
// foreign_keys(1) flip off "retention removes it on its own" needs the date,
// not just the mechanism.
const HourlyRetention = 90 * 24 * time.Hour

// CutoffDate renders the retention boundary as a plant-local YYYY-MM-DD
// string. Rows strictly BEFORE this date are eligible to be purged, so the
// window keeps the last olderThan of calendar dates plus today.
func CutoffDate(now time.Time, loc *time.Location, olderThan time.Duration) string {
	if loc == nil {
		loc = time.Local
	}
	return now.In(loc).Add(-olderThan).Format(DateLayout)
}

// RollUpDaily recomputes daily_counts from hourly_counts and reports how many
// daily rows were inserted or updated.
//
// IT RECOMPUTES RATHER THAN ACCUMULATES. Every pass re-derives the day total
// from the hours that are still there, so a delta that lands late — a
// confirmed anomaly released hours after the fact, an hour bucket backfilled
// by a catch-up poll — is picked up without anyone tracking a high-water mark.
// Verified against the restored Springfield database: 2,907 hourly rows roll
// into 248 daily rows and the grand total is 264,024 both before and after.
//
// A DAY GOES QUIET ON ITS OWN. Once PurgeRolledUpHourly has taken a date's
// hours, that date is no longer in the SELECT, so its daily row is simply not
// visited again — no delete, no revision, no bookkeeping.
//
// frozenBefore IS THE GUARD ON THE WORD "PERMANENT". The DO UPDATE carries a
// WHERE, so a daily row for a date older than the retention cutoff can never
// be rewritten. Without it there is a silent corruption path: an hourly row
// appearing for an already-purged date — which on a Pi means a backwards clock
// step, since these boxes have no battery-backed RTC and take their time from
// NTP after boot — would make the next pass recompute that day's total from
// the one stray row and overwrite years-old truth with it. The INSERT path is
// deliberately NOT guarded, so a date older than the cutoff that has never
// been summarised is still captured the first time this runs; that is the
// normal case on the first pass at an existing plant, where Springfield's
// hourly detail reaches back 96 days.
func RollUpDaily(db *sql.DB, frozenBefore string) (int64, error) {
	res, err := db.Exec(`INSERT INTO daily_counts (process_id, style_id, count_date, total)
		SELECT process_id, style_id, count_date, SUM(delta)
		  FROM hourly_counts
		 GROUP BY process_id, style_id, count_date
		ON CONFLICT(process_id, style_id, count_date) DO UPDATE SET
		       total      = excluded.total,
		       updated_at = datetime('now')
		 WHERE daily_counts.count_date >= ?`, frozenBefore)
	if err != nil {
		return 0, fmt.Errorf("roll up daily counts: %w", err)
	}
	return res.RowsAffected()
}

// PurgeRolledUpHourly deletes hourly_counts rows older than the cutoff date,
// but ONLY where daily_counts already holds that day's total. Returns the
// number deleted.
//
// The EXISTS clause is what makes the 90-day window safe rather than merely
// scheduled. Call order stops mattering, a rollup that failed this pass leaves
// nothing to delete, and a future refactor that moves the two calls apart
// cannot turn this into a plain DELETE. The failure direction is retention,
// never loss: if the rollup is broken, hourly detail accumulates and the log
// line in cmd/shingoedge/main.go says the rollup errored.
//
// NO COALESCE HERE, AND THAT IS CHECKED RATHER THAN ASSUMED. The sibling purge
// in retention.go needed one because `NOT (anomaly = 'jump' AND ...)` is
// three-valued over a nullable column and retained anomaly-NULL rows forever,
// invisibly. Every column this predicate touches is NOT NULL in the stored
// schema — verified on the Springfield database's own CREATE TABLE text, where
// hourly_counts declares process_id, style_id and count_date all NOT NULL —
// and EXISTS yields 0 or 1, never NULL. So the predicate is two-valued
// already. TestPurgeRolledUpHourly_NullSafe pins that rather than trusting it.
func PurgeRolledUpHourly(db *sql.DB, cutoffDate string) (int64, error) {
	res, err := db.Exec(`DELETE FROM hourly_counts
		WHERE count_date < ?
		  AND EXISTS (SELECT 1 FROM daily_counts d
		               WHERE d.process_id = hourly_counts.process_id
		                 AND d.style_id   = hourly_counts.style_id
		                 AND d.count_date = hourly_counts.count_date)`, cutoffDate)
	if err != nil {
		return 0, fmt.Errorf("purge rolled-up hourly counts: %w", err)
	}
	return res.RowsAffected()
}

// ListDaily returns daily totals for one process over an inclusive date range,
// newest first. This is the read that makes the hourly purge honest: once a
// date's hours are gone, this is where its production went.
//
// Rows are NOT joined to styles. A daily row outlives its style by design (see
// the daily_counts comment in schema/sqlite_ddl.go), and store/processes/
// styles.go's rule for reads — filter where the answer is "what may I pick
// now", never where it is "what was this" — puts this firmly in the second
// category.
func ListDaily(db *sql.DB, processID int64, fromDate, toDate string) ([]DailyCount, error) {
	rows, err := db.Query(`SELECT process_id, style_id, count_date, total
		FROM daily_counts
		WHERE process_id = ? AND count_date >= ? AND count_date <= ?
		ORDER BY count_date DESC, style_id`, processID, fromDate, toDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailyCount
	for rows.Next() {
		var d DailyCount
		if err := rows.Scan(&d.ProcessID, &d.StyleID, &d.CountDate, &d.Total); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
