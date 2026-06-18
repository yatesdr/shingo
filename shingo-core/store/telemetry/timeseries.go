package telemetry

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"shingocore/domain"
)

// Bucket is the time-bucket row type for the trend endpoint.
type Bucket = domain.TelemetryBucket

// GetTimeseries returns mission metrics bucketed by hour or day over the filter
// window (plan §3.B / §15.B). One row per non-empty bucket carries every metric
// the trend charts and hero sparklines need.
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
// bucket must be "hour" or "day"; anything else falls back to "hour".
func GetTimeseries(db *sql.DB, f Filter, bucket string) ([]Bucket, error) {
	if bucket != "hour" && bucket != "day" {
		bucket = "hour"
	}

	byBucket := map[time.Time]*Bucket{}
	var order []time.Time

	// 1) Outcome counts from orders, bucketed on the terminal timestamp.
	owhere, oargs := orderOutcomeWhere("", f)
	oargs = append(oargs, bucket)
	oBucketParam := len(oargs)
	countQuery := fmt.Sprintf(`SELECT date_trunc($%d, COALESCE(completed_at, updated_at)) AS b,
		COUNT(*),
		COUNT(*) FILTER (WHERE status='confirmed'),
		COUNT(*) FILTER (WHERE status='failed'),
		COUNT(*) FILTER (WHERE status IN ('cancelled','canceled'))
		FROM orders%s
		GROUP BY b ORDER BY b`, oBucketParam, owhere)
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
		order = append(order, b.BucketStart)
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
	dargs = append(dargs, bucket)
	dBucketParam := len(dargs)
	durQuery := fmt.Sprintf(`SELECT b,
		COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY exec_ms) FILTER (WHERE is_confirmed AND exec_ms > 0), 0)::BIGINT,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY exec_ms) FILTER (WHERE is_confirmed AND exec_ms > 0), 0)::BIGINT
		FROM (
			SELECT date_trunc($%d, core_completed) AS b,
				%s AS exec_ms,
				terminal_state IN ('FINISHED','delivered','confirmed') AS is_confirmed
			FROM mission_telemetry mt%s
		) q
		GROUP BY b ORDER BY b`, dBucketParam, executionMSExpr("mt"), dwhere)
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
		order = append(order, bt)
	}
	if err := drows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(order, func(i, j int) bool { return order[i].Before(order[j]) })
	out := make([]Bucket, 0, len(order))
	for _, t := range order {
		out = append(out, *byBucket[t])
	}
	return out, nil
}
