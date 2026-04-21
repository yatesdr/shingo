package store

// Phase 5b delegate file: shift CRUD now lives in store/shifts/.
// This file preserves the *store.DB method surface so external
// callers do not need to change.

import "shingoedge/store/shifts"

// Shift represents a work shift with start/end times.
type Shift = shifts.Shift

// ListShifts returns all shifts ordered by shift_number.
func (db *DB) ListShifts() ([]Shift, error) {
	return shifts.List(db.DB)
}

// UpsertShift inserts or replaces a shift by shift_number.
func (db *DB) UpsertShift(shiftNumber int, name, startTime, endTime string) error {
	return shifts.Upsert(db.DB, shiftNumber, name, startTime, endTime)
}

// DeleteShift removes a shift by shift_number.
func (db *DB) DeleteShift(shiftNumber int) error {
	return shifts.Delete(db.DB, shiftNumber)
}
