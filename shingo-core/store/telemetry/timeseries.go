package telemetry

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
)

// Bucket is the time-bucket row type for the trend endpoint.
type Bucket = domain.TelemetryBucket

// GetTimeseries returns mission metrics bucketed by hour or day over the
// filter window (plan §3.B / §15.B). One row per non-empty bucket carries
// every metric the trend charts and hero sparklines need, so a single call
// powers the whole 2×2 grid.
//
// bucket must be "hour" or "day"; callers validate before this point. It is
// passed as a query parameter to date_trunc (safe — validated, and bound not
// interpolated). SuccessRate is the bucket-level approximation documented on
// domain.TelemetryBucket.
func GetTimeseries(db *sql.DB, f Filter, bucket string) ([]Bucket, error) {
	if bucket != "hour" && bucket != "day" {
		bucket = "hour"
	}
	where, args := buildWhere(f)
	if where == "" {
		where = " WHERE core_completed IS NOT NULL"
	} else {
		where += " AND core_completed IS NOT NULL"
	}
	args = append(args, bucket)
	bucketParam := len(args) // date_trunc placeholder index (last arg)

	query := fmt.Sprintf(`SELECT
		date_trunc($%d, core_completed) AS b,
		COUNT(*),
		COUNT(*) FILTER (WHERE terminal_state IN ('FINISHED','delivered','confirmed')),
		COUNT(*) FILTER (WHERE terminal_state IN ('FAILED','failed')),
		COUNT(*) FILTER (WHERE terminal_state IN ('STOPPED','stopped','cancelled','canceled')),
		COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms > 0), 0)::BIGINT,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms > 0), 0)::BIGINT
		FROM mission_telemetry%s
		GROUP BY b ORDER BY b`, bucketParam, where)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.BucketStart, &b.Total, &b.Confirmed, &b.Failed,
			&b.Cancelled, &b.P50DurationMS, &b.P95DurationMS); err != nil {
			return nil, err
		}
		if denom := b.Confirmed + b.Failed; denom > 0 {
			b.SuccessRate = float64(b.Confirmed) / float64(denom) * 100
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
