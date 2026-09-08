package counters

// daily.go — the plant-local end of the counting ladder.
//
//	counter_snapshots  raw, one row per poll   14 days   retention.go
//	hourly_counts      per process/style/hour  permanent counters.go (UTC)
//	daily_counts       per process/style/day   permanent this file (plant-local)
//
// THE HOURLY PURGE IS GONE, and with it PurgeRolledUpHourly, HourlyRetention
// and CutoffDate. It was a deliberate, measured design — hour detail is a
// shift-shaped read, the day total is what survives the question — so the
// reversal owes a reason.
//
// The reason is that keeping the hours is what makes daily_counts honest.
// count_date is plant-local and cannot be anything else and still mean "the
// plant's day", so it is the one place a timezone still enters stored data.
// While the hours underneath survive, that is a cache: change the plant zone,
// re-run the roll-up, and the days are right. Purge the hours and it becomes
// the record — frozen in whatever zone the box believed at the time, which at
// Hopkinsville was the wrong one for three months.
//
// The cost is rows, and it was checked rather than assumed. A row appears only
// for an hour that actually produced: Hopkinsville carries 350 rows for three
// months, Springfield 2,907 over 70 dates (~41.5 a date). The 90-day window
// deleted 78 rows on the day it was measured — 1.14% of a restored database.
// Nobody ran that pass for the bytes, and nobody needs to now. If a
// forty-counter plant ever makes the growth real, retention comes back as a
// plain `DELETE WHERE bucket_start < ?` — with no zone in it at all, which is
// what made the old purge's EXISTS guard necessary in the first place.
//
// It also retires a documented regression: `?date=` on the production page is
// free text, so an engineer could ask for a date past the window and get an
// empty chart where the day total still existed. That trade is no longer made.

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

// DateLayout is the shape of daily_counts.count_date.
//
// IT IS A PLANT-LOCAL DATE, and it is now the ONLY plant-local thing stored
// anywhere in the counting ladder. hourly_counts moved to UTC bucket_start in
// 2026-09; a day, unlike an hour, cannot be stored zone-free and still mean
// "the plant's day", so this is where the zone necessarily lands. It stays
// honest because the hours underneath it are kept forever: a daily row is a
// CACHE of a plant-local grouping over UTC hours, and re-deriving it after a
// zone change is a rebuild, not a loss.
const DateLayout = "2006-01-02"

// RollUpDaily recomputes daily_counts from the UTC hour buckets, grouping them
// into PLANT-LOCAL calendar days, and reports how many daily rows were written.
//
// THE GROUPING IS DONE IN GO, NOT SQL, because SQLite has no IANA timezone
// support — it can shift by a fixed offset and nothing more, which is exactly
// wrong across a DST boundary. Reading the table costs nothing at the measured
// rate (Hopkinsville: 350 rows for three months; Springfield: 2,907 rows over
// 70 dates), and correctness across the two days a year that matter is not
// worth trading for a GROUP BY.
//
// IT RECOMPUTES RATHER THAN ACCUMULATES. Every pass re-derives each day total
// from the hours still present, so a delta that lands late — a confirmed
// anomaly released hours after the fact, a bucket backfilled by a catch-up
// poll — is picked up without anyone tracking a high-water mark.
//
// THERE IS NO frozenBefore GUARD ANY MORE, because the thing it guarded against
// is gone. It existed so that an hourly row appearing for an ALREADY-PURGED
// date could not make the next pass recompute that day from one stray row and
// overwrite years-old truth. Nothing is purged now, so a recompute always sees
// the whole day and can only reproduce it.
//
// Days written before the 2026-09 UTC migration are not visited at all: their
// hours live in hourly_counts_local_legacy, which nothing reads. Those daily
// rows therefore survive untouched, which is what preserves the pre-migration
// history the migration deliberately declined to reinterpret.
func RollUpDaily(db *sql.DB, loc *time.Location) (int64, error) {
	if loc == nil {
		loc = time.UTC
	}
	rows, err := db.Query(`SELECT process_id, style_id, bucket_start, delta FROM hourly_counts`)
	if err != nil {
		return 0, fmt.Errorf("roll up daily counts: read hourly: %w", err)
	}
	defer rows.Close()

	type key struct {
		process, style int64
		date           string
	}
	totals := make(map[key]int64)
	for rows.Next() {
		var processID, styleID, bucket, delta int64
		if err := rows.Scan(&processID, &styleID, &bucket, &delta); err != nil {
			return 0, fmt.Errorf("roll up daily counts: scan: %w", err)
		}
		date := time.Unix(bucket, 0).In(loc).Format(DateLayout)
		totals[key{processID, styleID, date}] += delta
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("roll up daily counts: iterate: %w", err)
	}

	var written int64
	for k, total := range totals {
		res, err := db.Exec(`INSERT INTO daily_counts (process_id, style_id, count_date, total)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(process_id, style_id, count_date) DO UPDATE SET
			       total      = excluded.total,
			       updated_at = datetime('now')`,
			k.process, k.style, k.date, total)
		if err != nil {
			return written, fmt.Errorf("roll up daily counts: upsert %s: %w", k.date, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return written, fmt.Errorf("roll up daily counts: rows affected: %w", err)
		}
		written += n
	}
	return written, nil
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
