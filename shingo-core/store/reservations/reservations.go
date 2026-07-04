// Package reservations manages Phase-1 bin reservations for the
// plan/apply → reservation-sourcing refactor.
//
// Acquire → Confirm → Release is the per-dispatch lifecycle.
// ReapOrphaned reclaims rows whose owning order is terminal/gone (owner-liveness backstop).
//
// Phase-0 stub bodies are replaced here. The v43 migration must have run
// before any Acquire call: the partial unique index uq_reservations_bin_active
// is what makes Acquire exactly-one-winner.
//
// REAPING IS OWNER-LIVENESS, NOT AGE (1c — D18-Q4 / D7). The 1a reaper used expires_at
// (a short ~60s TTL) as a proxy for "orphaned", valid only because Acquire→Confirm was
// milliseconds then. In 1c an order in 'sourcing' legitimately holds its reservations for
// minutes-to-hours (or days) while it waits for a source to appear — so age is no longer a
// proxy for orphaned. ReapOrphaned keys on the OWNING ORDER's liveness instead: a hold is
// reclaimed only when its order is terminal or gone, never on age. The expires_at column is
// still stamped at Acquire (it is NOT NULL) but is no longer read by any reaper — vestigial
// pending a schema drop. Do NOT re-introduce an age-based reap.
package reservations

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
)

// Execer is the minimal interface all functions in this package need.
// Both *sql.DB and *sql.Tx satisfy it, as does any store interface that
// exposes Exec (e.g. BinManifestStore). This avoids forcing callers to
// thread *sql.DB through every layer; they can pass whatever DB handle
// they already hold.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Queryer is the read counterpart of Execer, for the SELECT-returning helpers.
// Both *sql.DB and *sql.Tx satisfy it.
type Queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// Reservation is one row of the order's held reservations, as returned by
// ListByOrder. State is "pending" or "confirmed".
type Reservation struct {
	BinID int64
	State string
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
// Returns ErrReservationConflict when an active (pending or confirmed)
// reservation already exists on binID. The unique index uq_reservations_bin_active
// is keyed on bin_id ALONE, so this fires even when THIS SAME order already holds
// the bin — re-Acquiring your own hold conflicts on its own row. Callers that
// retry across ticks (the 1c plan-time reserve/reconcile) MUST therefore
// load-held-first and skip Acquire for bins they already hold, or they will
// report their own held bins as "missing" every tick. A conflict on a bin the
// caller does NOT already hold is a genuine race lost to another order.
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

// ListByOrder returns all reservations held by orderID, both pending and
// confirmed. The 1c plan-time reconcile loads its own holds with this BEFORE
// deciding what to keep / release / acquire — the owner-aware step that dodges
// the per-bin unique-index self-conflict documented on Acquire.
func ListByOrder(db Queryer, orderID int64) ([]Reservation, error) {
	rows, err := db.Query(
		`SELECT bin_id, state FROM reservations WHERE order_id=$1 ORDER BY bin_id`,
		orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("reservations list-by-order: %w", err)
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.BinID, &r.State); err != nil {
			return nil, fmt.Errorf("reservations list-by-order scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReapOrphaned deletes reservation rows — in BOTH states — whose owning order is
// terminal or no longer exists. This is the 1c owner-liveness reaper (D18-Q4 / D7):
// reclamation keys on the OWNER being dead, NEVER on the hold's age. A hold under a live,
// non-terminal order is sacred no matter how long it has been held — an order in sourcing
// legitimately waits minutes-to-hours (or days) for its source to appear; demand is
// operator-driven and never evaporates.
//
// It is the defense-in-depth backstop BEHIND the terminal chokepoint: TerminalizeOrder
// (store/orders.go) already releases an order's reservations in the same tx that takes it
// terminal, so on the normal path there is nothing here to reap. This catches rows that
// leaked past that path — a crash between the status write and the release, or a raw
// status bypass. Idempotent with the chokepoint: a row already released there simply isn't
// present.
//
// The `order_id NOT IN (orders)` leg is currently unreachable — reservations.order_id is a
// RESTRICT foreign key (migrations.go v42) and orders are never hard-deleted, so a
// reservation can never outlive its order row — but is kept as one-clause insurance against
// a future ON DELETE CASCADE. Returns the number of rows deleted; errors are non-fatal to
// the caller (the reconciliation loop logs and continues).
func ReapOrphaned(db Execer) (int, error) {
	result, err := db.Exec(fmt.Sprintf(
		`DELETE FROM reservations
		 WHERE order_id IN (SELECT id FROM orders WHERE status IN (%s))
		    OR order_id NOT IN (SELECT id FROM orders)`,
		protocol.TerminalStatusSQLList()))
	if err != nil {
		return 0, fmt.Errorf("reservations reap-orphaned: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
