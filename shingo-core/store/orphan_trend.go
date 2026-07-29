package store

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

// orphan_trend.go — Stage 5.7's reads: the orphan rate over time, and the
// per-site lane.
//
// ── WHY NEW QUERIES AND NOT ListOrphanFindings ───────────────────────────────
//
// ListOrphanFindings returns EVERY orphan as an individual row, ordered
// orphan_aged_at NULLS FIRST. It is unbounded on purpose — "if the bucket is
// large, that IS the finding" — and it answers the LEVEL plus the fresh/aged
// split. The plan says the number that matters is the TREND, and a level is not
// a trend at any row count. Summarising that list in Go would also put an
// unbounded result set on a web page, which is the one thing its own doc
// comment warns the LIMIT would have hidden.
//
// So the trend gets an aggregate that is bounded by BUCKET COUNT and the lane
// gets one bounded by STATION COUNT, and ListOrphanFindings keeps its job as
// the full drill-down for anyone who wants every row.
//
// ── THE TREND IS KEYED ON created_at ─────────────────────────────────────────
//
// See domain.OrphanBucket, which carries the four reasons and corrects
// AgeOutOrphanOrders' comment. The short version: the rate's denominator is
// "all orders in this bucket", and created_at is the only timestamp every order
// has.

// OrphanBucket and OrphanSite are domain-owned; www may not import store.
type (
	OrphanBucket = domain.OrphanBucket
	OrphanSite   = domain.OrphanSite
)

// OrphanRateBuckets returns per-bucket orphan and total order counts since
// `since`, keyed on orders.created_at.
//
// ONLY NON-EMPTY BUCKETS COME BACK. A GROUP BY cannot emit a bucket that has no
// rows, and generate_series here would move the "an empty bucket is unmeasured,
// not zero" rule into SQL where no unit test can reach it. www.BuildOrphanTrend
// generates the full series and renders the gaps, and it is tested for exactly
// that.
//
// COUNT(*) FILTER rather than two queries or a subquery: the numerator and the
// denominator must come from ONE scan of one predicate, or a row created
// between two queries lands in one and not the other and the rate is wrong by a
// race nobody will reproduce.
func (db *DB) OrphanRateBuckets(since time.Time, bucket time.Duration) ([]OrphanBucket, error) {
	secs := bucket.Seconds()
	if secs <= 0 {
		return nil, fmt.Errorf("orphan rate buckets: bucket width must be positive, got %s", bucket)
	}

	// The epoch arithmetic is cast to double precision explicitly at both
	// sites. extract(epoch ...) returns numeric on PG 14+ and double precision
	// before it, and to_timestamp takes double precision — left implicit, the
	// query resolves differently across the versions a plant and the dev stack
	// might be running.
	rows, err := db.Query(`
		SELECT to_timestamp(
		           floor(extract(epoch FROM created_at)::double precision / $1::double precision)
		           * $1::double precision) AS bucket_start,
		       COUNT(*) FILTER (WHERE origin_class = $2) AS orphans,
		       COUNT(*) AS orders
		  FROM orders
		 WHERE created_at >= $3
		 GROUP BY bucket_start
		 ORDER BY bucket_start`, secs, protocol.OriginClassOrphan, since)
	if err != nil {
		return nil, fmt.Errorf("orphan rate buckets: %w", err)
	}
	defer rows.Close()

	var out []OrphanBucket
	for rows.Next() {
		var b OrphanBucket
		if err := rows.Scan(&b.Start, &b.Orphans, &b.Orders); err != nil {
			return nil, fmt.Errorf("scan orphan bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SummarizeOrphansBySite returns the orphan lane: per-station live and aged
// counts, worst first.
//
// LIVE AND AGED ARE COUNTED IN ONE PASS with FILTER, for the same reason the
// trend's two counts are: two queries over a table a sweep is actively stamping
// can disagree with each other, and a lane whose Live + Aged does not equal its
// Total is a lane nobody will trust again.
//
// OldestLive is MIN(created_at) over the live rows only, and comes back NULL
// when a station has none — carried as a pointer so "no live findings" cannot
// be confused with a zero time.
//
// Bounded by station count, which is the plant's cell count. Unlike
// ListOrphanFindings this is safe to render whole.
func (db *DB) SummarizeOrphansBySite() ([]OrphanSite, error) {
	rows, err := db.Query(`
		SELECT station_id,
		       COUNT(*) FILTER (WHERE orphan_aged_at IS NULL)     AS live,
		       COUNT(*) FILTER (WHERE orphan_aged_at IS NOT NULL) AS aged,
		       COUNT(*)                                           AS total,
		       MIN(created_at) FILTER (WHERE orphan_aged_at IS NULL) AS oldest_live
		  FROM orders
		 WHERE origin_class = $1
		 GROUP BY station_id
		 ORDER BY live DESC, total DESC, station_id`, protocol.OriginClassOrphan)
	if err != nil {
		return nil, fmt.Errorf("summarize orphans by site: %w", err)
	}
	defer rows.Close()

	var out []OrphanSite
	for rows.Next() {
		var s OrphanSite
		if err := rows.Scan(&s.StationID, &s.Live, &s.Aged, &s.Total, &s.OldestLive); err != nil {
			return nil, fmt.Errorf("scan orphan site: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
