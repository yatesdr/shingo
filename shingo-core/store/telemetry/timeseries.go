package telemetry

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"shingo/protocol/clock"
	"shingocore/domain"
)

// Bucket is the time-bucket row type for the trend endpoint.
type Bucket = domain.TelemetryBucket

// GetTimeseries returns mission metrics bucketed by hour or day over the filter
// window (plan §3.B / §15.B). One row per bucket carries every metric the trend
// charts and hero sparklines need.
//
// COUNTS (total/confirmed/failed/cancelled + success_rate) come from the orders
// table — the complete terminal record (order_outcome.go) — bucketed on the
// terminal timestamp COALESCE(completed_at, updated_at). Execution P50/P95 come
// from mission_telemetry (robot missions only, Q-031) and are merged in by
// bucket. Sourcing counts from orders is what makes the success-rate line
// honest for windows where failures terminated inside Core without ever
// becoming a vendor mission (the old mission_telemetry-only path, blind to
// those, read a flat 100%).
//
// Buckets are PLANT hours and days: loc is the plant zone, passed to SQL as a
// bound parameter. The session is pinned to UTC (store.pgxConnConfig), so a
// bare date_trunc('day', ts) cut the plant's day at UTC midnight — 19:00 CDT at
// a Central plant — and every day bar carried the previous evening.
//
// The series is CONTINUOUS (fillBuckets): every bucket from the window's start
// to the earlier of its end and now is present, and an empty one is a measured
// zero. A chart that skipped empty hours drew 00, 02, 04, 05 on its axis.
//
// bucket must be "hour" or "day"; anything else falls back to "hour".
func GetTimeseries(db *sql.DB, f Filter, bucket string, loc *time.Location) ([]Bucket, error) {
	if bucket != "hour" && bucket != "day" {
		bucket = "hour"
	}
	if loc == nil {
		loc = time.UTC
	}
	tz := loc.String()

	byBucket := map[time.Time]*Bucket{}

	// 1) Outcome counts from orders, bucketed on the terminal timestamp.
	owhere, oargs := orderOutcomeWhere("", f)
	oargs = append(oargs, bucket, tz)
	oBucketParam := len(oargs) - 1
	countQuery := fmt.Sprintf(`SELECT date_trunc($%d, COALESCE(completed_at, updated_at) AT TIME ZONE $%d) AT TIME ZONE $%d AS b,
		COUNT(*),
		COUNT(*) FILTER (WHERE status='confirmed'),
		COUNT(*) FILTER (WHERE status='failed'),
		COUNT(*) FILTER (WHERE status IN ('cancelled','canceled'))
		FROM orders%s
		GROUP BY b ORDER BY b`, oBucketParam, oBucketParam+1, oBucketParam+1, owhere)
	crows, err := db.Query(countQuery, oargs...)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var b Bucket
		if err := crows.Scan(&b.BucketStart, &b.Total, &b.Confirmed, &b.Failed, &b.Cancelled); err != nil {
			crows.Close()
			return nil, err
		}
		if denom := b.Confirmed + b.Failed; denom > 0 {
			b.SuccessRate = float64(b.Confirmed) / float64(denom) * 100
		}
		bb := b
		byBucket[b.BucketStart] = &bb
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, err
	}
	crows.Close()

	// 2) Execution P50/P95 from mission_telemetry, same bucket grain. Confirmed
	// missions with exec_ms>0 only (Q-031); exec_ms computed once per row.
	dwhere, dargs := buildWhere(f)
	if dwhere == "" {
		dwhere = " WHERE core_completed IS NOT NULL"
	} else {
		dwhere += " AND core_completed IS NOT NULL"
	}
	dargs = append(dargs, bucket, tz)
	dBucketParam := len(dargs) - 1
	durQuery := fmt.Sprintf(`SELECT b,
		COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY exec_ms) FILTER (WHERE is_confirmed AND exec_ms > 0), 0)::BIGINT,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY exec_ms) FILTER (WHERE is_confirmed AND exec_ms > 0), 0)::BIGINT
		FROM (
			SELECT date_trunc($%d, core_completed AT TIME ZONE $%d) AT TIME ZONE $%d AS b,
				%s AS exec_ms,
				terminal_state IN ('FINISHED','delivered','confirmed') AS is_confirmed
			FROM mission_telemetry mt%s
		) q
		GROUP BY b ORDER BY b`, dBucketParam, dBucketParam+1, dBucketParam+1, executionMSExpr("mt"), dwhere)
	drows, err := db.Query(durQuery, dargs...)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var bt time.Time
		var p50, p95 int64
		if err := drows.Scan(&bt, &p50, &p95); err != nil {
			return nil, err
		}
		if b, ok := byBucket[bt]; ok {
			b.P50DurationMS, b.P95DurationMS = p50, p95
			continue
		}
		// Defensive: a duration bucket with no orders bucket shouldn't happen
		// (every mission has an order), but keep it rather than silently drop.
		byBucket[bt] = &Bucket{BucketStart: bt, P50DurationMS: p50, P95DurationMS: p95}
	}
	if err := drows.Err(); err != nil {
		return nil, err
	}

	return fillBuckets(byBucket, f, bucket, loc, clock.Now()), nil
}

// fillBuckets lays the rows onto a continuous series of plant buckets from the
// window's start to the earlier of its end and now, zero-filling the gaps. Day
// steps go through AddDate in loc, so a 25-hour fall-back day is one bucket
// and the next starts on the plant's midnight, not 24 hours later. A row
// outside the generated span (an unbounded window) is kept, never dropped: the
// fill may add zeros, it may not lose a count.
func fillBuckets(byBucket map[time.Time]*Bucket, f Filter, bucket string, loc *time.Location, now time.Time) []Bucket {
	keyed := make(map[int64]*Bucket, len(byBucket))
	for t, b := range byBucket {
		keyed[t.Unix()] = b
	}
	var starts []time.Time
	if f.Since != nil {
		end := now
		if f.Until != nil && f.Until.Before(end) {
			end = *f.Until
		}
		for t := truncateIn(*f.Since, bucket, loc); !t.After(end); t = nextBucket(t, bucket, loc) {
			starts = append(starts, t)
		}
	}
	seen := make(map[int64]bool, len(starts))
	for _, t := range starts {
		seen[t.Unix()] = true
	}
	for k, b := range keyed {
		if !seen[k] {
			starts = append(starts, b.BucketStart)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	out := make([]Bucket, 0, len(starts))
	for _, t := range starts {
		if b, ok := keyed[t.Unix()]; ok {
			out = append(out, *b)
			continue
		}
		out = append(out, Bucket{BucketStart: t.UTC()})
	}
	return out
}

func truncateIn(t time.Time, bucket string, loc *time.Location) time.Time {
	l := t.In(loc)
	if bucket == "day" {
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	}
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
}

func nextBucket(t time.Time, bucket string, loc *time.Location) time.Time {
	if bucket == "day" {
		l := t.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc)
	}
	return t.Add(time.Hour)
}
