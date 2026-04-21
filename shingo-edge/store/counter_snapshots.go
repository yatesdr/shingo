package store

// Phase 5b delegate file: counter_snapshots CRUD now lives in
// store/counters/. This file preserves the *store.DB method surface so
// external callers do not need to change.

import "shingoedge/store/counters"

// CounterSnapshot records a PLC counter reading.
type CounterSnapshot = counters.Snapshot

// InsertCounterSnapshot writes one counter_snapshots row.
func (db *DB) InsertCounterSnapshot(rpID int64, countValue, delta int64, anomaly string, confirmed bool) (int64, error) {
	return counters.InsertSnapshot(db.DB, rpID, countValue, delta, anomaly, confirmed)
}

// ListUnconfirmedAnomalies returns every counter snapshot tagged as a
// "jump" anomaly that the operator has not yet confirmed.
func (db *DB) ListUnconfirmedAnomalies() ([]CounterSnapshot, error) {
	return counters.ListUnconfirmedAnomalies(db.DB)
}

// ConfirmAnomaly marks an anomaly snapshot as operator-confirmed.
func (db *DB) ConfirmAnomaly(id int64) error {
	return counters.ConfirmAnomaly(db.DB, id)
}

// DismissAnomaly deletes an unconfirmed anomaly snapshot.
func (db *DB) DismissAnomaly(id int64) error {
	return counters.DismissAnomaly(db.DB, id)
}
