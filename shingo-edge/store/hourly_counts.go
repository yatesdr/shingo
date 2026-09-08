package store

// Phase 5b delegate file: hourly_counts CRUD now lives in
// store/counters/. This file preserves the *store.DB method surface so
// external callers do not need to change.

import "shingoedge/store/counters"

// UpsertHourlyCount adds delta to the UTC hour bucket, or inserts it if new.
func (db *DB) UpsertHourlyCount(processID, styleID, bucketStart, delta int64) error {
	return counters.UpsertHourly(db.DB, processID, styleID, bucketStart, delta)
}

// ListHourlyCounts returns the rows for one process/style whose buckets fall
// in the half-open UTC range [from, to).
func (db *DB) ListHourlyCounts(processID, styleID, from, to int64) ([]counters.HourlyCount, error) {
	return counters.ListHourly(db.DB, processID, styleID, from, to)
}

// HourlyCountTotals returns per-bucket totals for a process over the half-open
// UTC range [from, to), keyed by bucket_start and summed across styles.
func (db *DB) HourlyCountTotals(processID, from, to int64) (map[int64]int64, error) {
	return counters.HourlyTotals(db.DB, processID, from, to)
}
