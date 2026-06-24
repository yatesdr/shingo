// Package reservations manages Phase-1 bin reservations for the
// plan/apply → reservation-sourcing refactor.
//
// All four functions are stubs in Phase 0: the table exists (v42 migration)
// but no production code calls into this package yet. Phase 1 replaces
// the stub bodies with real SQL and wires callers into ClaimForDispatch.
package reservations

import (
	"database/sql"
	"errors"
	"time"
)

var errNotImplemented = errors.New("reservations: not implemented (Phase-0 stub)")

// Acquire inserts a reservation row for the given (orderID, binID) pair in
// state "pending". reservedBy identifies the dispatch path; reason is a
// human-readable tag. expiresAt is the absolute wall-clock deadline after
// which Expire may reclaim the hold.
func Acquire(db *sql.DB, orderID, binID int64, reservedBy, reason string, expiresAt time.Time) error {
	return errNotImplemented
}

// Confirm transitions the (orderID, binID) reservation from "pending" to
// "confirmed", recording that the physical claim succeeded.
func Confirm(db *sql.DB, orderID, binID int64) error {
	return errNotImplemented
}

// Release deletes the reservation for (orderID, binID), freeing the bin for
// future reservation attempts.
func Release(db *sql.DB, orderID, binID int64) error {
	return errNotImplemented
}

// Expire removes all reservation rows whose expires_at has passed.
// Returns the number of rows deleted and any error.
func Expire(db *sql.DB) (int, error) {
	return 0, errNotImplemented
}
