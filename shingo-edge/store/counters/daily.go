package counters

// daily.go — the day view over the hour buckets.
//
//	counter_snapshots  raw, one row per poll   14 days     retention.go
//	hourly_counts      per process/style/hour  permanent   counters.go (UTC)
//	daily_counts       FROZEN pre-2026-09 rollup, read-only, this file
//
// A DAY IS DERIVED, NOT STORED. Grouping UTC hour buckets into plant-local days
// happens in ListDaily, at read time, so NO stored row anywhere in the counting
// ladder carries a timezone. Changing the plant clock changes what the reads
// answer, with nothing to migrate and no job to re-run — which is the property
// the whole 2026-09 move was for, and which a cached day total would have given
// straight back by baking a zone into its key.
//
// THE ROLLUP AND THE HOURLY PURGE ARE BOTH GONE, and both were deliberate
// designs, so the reversals owe reasons.
//
// The purge (PurgeRolledUpHourly, HourlyRetention, CutoffDate) existed to bound
// growth. But a row appears only for an hour that actually produced — 350 rows
// for three months at Hopkinsville, 2,907 over 70 dates at Springfield — and
// keeping the hours is what makes a day derivable at all. If a forty-counter
// plant ever makes the growth real, retention returns as a plain
// `DELETE WHERE bucket_start < ?`, with no zone in it, which is what made the
// old purge's EXISTS guard necessary in the first place.
//
// RollUpDaily followed it: once the hours are permanent, a stored day total is
// a second copy of the same truth with a timezone in its key, and second copies
// drift. Deriving costs a range query over a table measured in hundreds of rows.
//
// Deleting the purge also retired a documented regression: `?date=` on the
// production page is free text, so an engineer could ask for a date past the
// 90-day window and get an empty chart where the day total still existed.
//
// daily_counts SURVIVES AS HISTORY ONLY. It holds the day totals written before
// the migration, whose hour detail sits in hourly_counts_local_legacy and is
// deliberately not reinterpreted. ListDaily reads it only where the buckets
// answer nothing for that (style, date), so it can never shadow a live day.

import (
	"database/sql"
	"fmt"
	"sort"
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

// ListDaily returns daily totals for one process over an inclusive PLANT-LOCAL
// date range, newest first.
//
// IT DERIVES, IT DOES NOT READ A ROLLUP. The day totals are grouped out of the
// UTC hour buckets at read time, in the plant's zone. That is what keeps the
// zone out of stored data entirely: a day is a QUESTION asked of the hours, not
// a fact written down, so changing the plant zone changes the answer with
// nothing to migrate and nothing to re-run. The rollup job and its stored
// upsert are gone for that reason — a cached day total is a second copy of the
// truth with a timezone baked into its key, which is the whole defect this work
// exists to remove.
//
// Deriving is affordable because a bucket row exists only for an hour that
// actually produced: 350 rows for three months at Hopkinsville, 2,907 over 70
// dates at Springfield. A range query over that is not worth caching.
//
// daily_counts IS STILL READ, FOR PRE-MIGRATION DATES ONLY. It is frozen —
// nothing has written it since the 2026-09 UTC migration — and it holds the day
// totals for the period whose hour detail lives in hourly_counts_local_legacy,
// which is deliberately not reinterpreted. A stored row is used only where the
// hours produce nothing for that (style, date), so post-migration days always
// come from the buckets and history still answers.
//
// Rows are NOT joined to styles. A daily row outlives its style by design (see
// the daily_counts comment in schema/sqlite_ddl.go), and store/processes/
// styles.go's rule for reads — filter where the answer is "what may I pick
// now", never where it is "what was this" — puts this firmly in the second
// category.
func ListDaily(db *sql.DB, processID int64, fromDate, toDate string, loc *time.Location) ([]DailyCount, error) {
	if loc == nil {
		loc = time.UTC
	}
	from, _, err := DayBounds(fromDate, loc)
	if err != nil {
		return nil, err
	}
	_, to, err := DayBounds(toDate, loc)
	if err != nil {
		return nil, err
	}

	type key struct {
		style int64
		date  string
	}
	derived := make(map[key]int64)

	rows, err := db.Query(`SELECT style_id, bucket_start, delta FROM hourly_counts
		WHERE process_id = ? AND bucket_start >= ? AND bucket_start < ?`,
		processID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list daily: read hourly: %w", err)
	}
	for rows.Next() {
		var styleID, bucket, delta int64
		if err := rows.Scan(&styleID, &bucket, &delta); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list daily: scan hourly: %w", err)
		}
		derived[key{styleID, time.Unix(bucket, 0).In(loc).Format(DateLayout)}] += delta
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list daily: iterate hourly: %w", err)
	}
	rows.Close()

	out := make([]DailyCount, 0, len(derived))
	for k, total := range derived {
		out = append(out, DailyCount{ProcessID: processID, StyleID: k.style, CountDate: k.date, Total: total})
	}

	// Pre-migration history. A stored row loses to a derived one, so a date the
	// buckets cover is answered by the buckets even if a stale rollup row for it
	// still exists.
	legacy, err := db.Query(`SELECT style_id, count_date, total FROM daily_counts
		WHERE process_id = ? AND count_date >= ? AND count_date <= ?`,
		processID, fromDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("list daily: read frozen rollup: %w", err)
	}
	defer legacy.Close()
	for legacy.Next() {
		var styleID, total int64
		var date string
		if err := legacy.Scan(&styleID, &date, &total); err != nil {
			return nil, fmt.Errorf("list daily: scan frozen rollup: %w", err)
		}
		if _, ok := derived[key{styleID, date}]; ok {
			continue
		}
		out = append(out, DailyCount{ProcessID: processID, StyleID: styleID, CountDate: date, Total: total})
	}
	if err := legacy.Err(); err != nil {
		return nil, fmt.Errorf("list daily: iterate frozen rollup: %w", err)
	}

	// Newest first, then style — the order the old SQL ORDER BY produced.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CountDate != out[j].CountDate {
			return out[i].CountDate > out[j].CountDate
		}
		return out[i].StyleID < out[j].StyleID
	})
	return out, nil
}
