package store

// Phase 5b delegate file: hourly_counts CRUD now lives in
// store/counters/. This file preserves the *store.DB method surface so
// external callers do not need to change.

import "shingoedge/store/counters"

// HourlyCount represents accumulated production count for one hour.
type HourlyCount = counters.HourlyCount

// UpsertHourlyCount adds delta to the existing count for the given
// process/style/date/hour, or inserts a new row if none exists.
func (db *DB) UpsertHourlyCount(processID, styleID int64, countDate string, hour int, delta int64) error {
	return counters.UpsertHourly(db.DB, processID, styleID, countDate, hour, delta)
}

// ListHourlyCounts returns all hourly count rows for a given
// process/style/date.
func (db *DB) ListHourlyCounts(processID, styleID int64, countDate string) ([]HourlyCount, error) {
	return counters.ListHourly(db.DB, processID, styleID, countDate)
}

// HourlyCountTotals returns per-hour totals for a process/date, summed
// across all styles.
func (db *DB) HourlyCountTotals(processID int64, countDate string) (map[int]int64, error) {
	return counters.HourlyTotals(db.DB, processID, countDate)
}
