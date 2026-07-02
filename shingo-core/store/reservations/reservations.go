// Package reservations manages Phase-1 bin reservations for the
// plan/apply → reservation-sourcing refactor.
//
// Acquire → Confirm → Release is the per-dispatch lifecycle.
// Expire reclaims pending rows whose TTL has passed (crash-leak backstop).
//
// Phase-0 stub bodies are replaced here. The v43 migration must have run
// before any Acquire call: the partial unique index uq_reservations_bin_active
// is what makes Acquire exactly-one-winner.
//
// ⚠ PHASE-1A CAVEAT ON Expire:
// Expire uses expires_at (stamped at Acquire time with a short TTL ~60s)
// as a proxy for "orphaned". This is valid in 1a because Acquire→Confirm
// is a single function call (milliseconds), so a pending row older than
// the TTL is almost certainly a crash-leak — not a legitimately held
// reservation. In Phase 1c, orders in 'sourcing' will legitimately hold
// reservations for minutes-to-hours while waiting. Phase 1c MUST replace
// this age-based reap with an order-liveness check: only reap a pending
// hold whose owning order is terminal/gone. Do NOT inherit the age-proxy
// logic into 1c.
package reservations

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/shared/clock"
)

// Execer is the minimal interface all functions in this package need.
// Both *sql.DB and *sql.Tx satisfy it, as does any store interface that
// exposes Exec (e.g. BinManifestStore). This avoids forcing callers to
// thread *sql.DB through every layer; they can pass whatever DB handle
// they already hold.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// ErrReservationConflict is returned by Acquire when another order already
// holds an active (pending or confirmed) reservation on the requested bin.
// Callers should treat this as a transient race: the losing order requeues
// and the scanner retries on the next tick.
var ErrReservationConflict = fmt.Errorf("reservations: bin already reserved (race)")

// Acquire inserts a reservation row for (orderID, binID) in state "pending".
// expiresAt is the absolute deadline after which Expire may reclaim the hold
// if the Confirm step was never reached (crash-leak backstop).
//
// Returns ErrReservationConflict when another order already holds an active
// (pending or confirmed) reservation on binID — the caller lost the race.
// Returns any other non-nil error for transient DB failures.
func Acquire(db Execer, orderID, binID int64, reservedBy, reason string, expiresAt time.Time) error {
	result, err := db.Exec(
		`INSERT INTO reservations (order_id, bin_id, state, reserved_by, reason, expires_at)
		 VALUES ($1, $2, 'pending', $3, $4, $5)
		 ON CONFLICT DO NOTHING`,
		orderID, binID, reservedBy, reason, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("reservations acquire: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		// ON CONFLICT DO NOTHING suppressed the insert — another order already
		// holds an active reservation on this bin via the uq_reservations_bin_active index.
		return ErrReservationConflict
	}
	return nil
}

// Confirm transitions the (orderID, binID) reservation from "pending" to
// "confirmed", recording that the physical bin claim succeeded. A no-op if
// the row is already confirmed (idempotent for retry safety).
func Confirm(db Execer, orderID, binID int64) error {
	_, err := db.Exec(
		`UPDATE reservations SET state='confirmed'
		 WHERE order_id=$1 AND bin_id=$2 AND state='pending'`,
		orderID, binID,
	)
	if err != nil {
		return fmt.Errorf("reservations confirm: %w", err)
	}
	return nil
}

// Release deletes the reservation for (orderID, binID), freeing the bin for
// future reservations. Safe to call even when no row exists (idempotent).
func Release(db Execer, orderID, binID int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations WHERE order_id=$1 AND bin_id=$2`,
		orderID, binID,
	)
	if err != nil {
		return fmt.Errorf("reservations release: %w", err)
	}
	return nil
}

// ReleaseByOrder deletes all reservations for the given order, both pending
// and confirmed. Used by teardown paths (TerminalizeOrder) to ensure no holds
// outlive the order that owns them.
func ReleaseByOrder(db Execer, orderID int64) error {
	_, err := db.Exec(`DELETE FROM reservations WHERE order_id=$1`, orderID)
	if err != nil {
		return fmt.Errorf("reservations release-by-order: %w", err)
	}
	return nil
}

// ReleaseByBin deletes any reservation on binID, in whatever state. Called at
// the delivered moment (ApplyArrival / ApplyMultiBinArrival) in the same tx that
// clears bins.claimed_by, so a bin's reservation lives exactly as long as its
// claim: the bin frees for re-reservation at delivery instead of lingering until
// the owning order's terminal transition. Bin-keyed because the arrival path is
// bin-centric, and the uq_reservations_bin_active index guarantees at most one
// active row per bin (so this deletes exactly the delivering order's hold).
func ReleaseByBin(db Execer, binID int64) error {
	_, err := db.Exec(`DELETE FROM reservations WHERE bin_id=$1`, binID)
	if err != nil {
		return fmt.Errorf("reservations release-by-bin: %w", err)
	}
	return nil
}

// Expire removes pending reservation rows whose expires_at has passed.
// Uses clock.Now() (sim-time in sim, wall-time in prod) as the cutoff —
// matching AbandonStuckOrders — so the reaper fires correctly in sim
// regardless of sim-clock speed.
//
// Only 'pending' rows are reaped; 'confirmed' rows are owned by the order
// lifecycle and are cleared by ReleaseByOrder on teardown.
//
// Returns the number of rows deleted and any error. Errors are non-fatal
// to the caller (the reconciliation loop logs and continues).
//
// ⚠ 1a proxy: expires_at is a proxy for "orphaned" because Acquire→Confirm
// is milliseconds in 1a; a row still pending past its TTL is a crash-leak.
// Phase 1c must replace this with order-liveness gating before extending
// reservation TTLs to minutes-or-hours. See package-level caveat.
func Expire(db Execer) (int, error) {
	cutoff := clock.Now().UTC()
	result, err := db.Exec(
		`DELETE FROM reservations WHERE state='pending' AND expires_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("reservations expire: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
