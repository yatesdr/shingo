// Package reservations manages Phase-1 bin reservations for the
// plan/apply → reservation-sourcing refactor.
//
// Acquire → Confirm → Release is the per-dispatch lifecycle.
// ReapOrphaned reclaims rows whose owning order is terminal/gone (owner-liveness backstop).
//
// RELEASE IS A HARD DELETE. Release/ReleaseByOrder/ReleaseByBin/ReleaseByNode and
// ReapOrphaned all DELETE rows — a reservation never transitions to a terminal
// state. So every row on disk is 'pending' or 'confirmed', which is why the partial
// unique indexes' `state IN ('pending','confirmed')` predicate matches every row
// (kept future-proof, and since v44 also pinned by a CHECK).
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

// Compile-time proof of what the doc comments promise — a signature drift on the
// concrete DB handles becomes a build error, not a runtime one.
var (
	_ Execer  = (*sql.DB)(nil)
	_ Execer  = (*sql.Tx)(nil)
	_ Queryer = (*sql.DB)(nil)
	_ Queryer = (*sql.Tx)(nil)
)

// State is a reservation's lifecycle state. No longer free-text: v44 adds a CHECK
// pinning the column to these values, so a typo can no longer silently escape the
// partial unique index (D45).
type State string

const (
	StatePending   State = "pending"
	StateConfirmed State = "confirmed"
)

// Kind is the resource a reservation covers — the resource_kind column's domain
// (v44). 'mouth' is accepted by the schema but has no dedicated code path in 1d.
type Kind string

const (
	KindBin  Kind = "bin"
	KindSlot Kind = "slot"
)

// Ref is the kind-agnostic identity of a reserved resource: a bin (Kind=bin,
// ID=bins.id) or a slot (Kind=slot, ID=nodes.id). Every primitive keys on a Ref —
// the seed of the future Claim/Handle aggregate (D45 §4). The exactly-one-of CHECK
// + per-kind partial indexes make (Kind, ID) a row's identity. Callers build one
// with BinRef/SlotRef so a call site reads its kind at a glance.
type Ref struct {
	Kind Kind
	ID   int64
}

// BinRef / SlotRef construct the two Refs 1d uses.
func BinRef(binID int64) Ref   { return Ref{Kind: KindBin, ID: binID} }
func SlotRef(nodeID int64) Ref { return Ref{Kind: KindSlot, ID: nodeID} }

// Reservation is one row of the order's held reservations, as returned by
// ListByOrder. Exactly one of BinID/NodeID is set, per Kind (the other is 0).
type Reservation struct {
	Kind   Kind
	BinID  int64 // bins.id for a bin reservation; 0 for slot/mouth
	NodeID int64 // nodes.id for a slot/mouth reservation; 0 for bin
	State  State
}

// ErrReservationConflict is returned by Acquire/AcquireSlot when another order
// already holds an active (pending or confirmed) reservation on the requested
// resource (bin or slot). Callers should treat this as a transient race: the
// losing order requeues and the scanner retries on the next tick.
var ErrReservationConflict = fmt.Errorf("reservations: resource already reserved (race)")

// Acquire inserts a bin reservation row for (orderID, binID) in state "pending".
// reservedBy is the actor tag for forensics. (The former reason + expiresAt params
// are gone in v44 — reason was always "", and expires_at is retired as a reaping
// key: reaping keys on the owner's liveness, not age, D18-Q4.)
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
func Acquire(db Execer, orderID, binID int64, reservedBy string) error {
	return acquire(db, orderID, BinRef(binID), reservedBy)
}

// AcquireSlot is Acquire for a destination slot: a pending slot reservation on
// nodeID. Conflict via uq_reservations_slot_active (one active slot row per node).
// Occupancy is NOT consulted here — a slot that physically holds a bin is still
// reservable (D29); the NOT EXISTS(bins) check lives at confirm/claim time only.
func AcquireSlot(db Execer, orderID, nodeID int64, reservedBy string) error {
	return acquire(db, orderID, SlotRef(nodeID), reservedBy)
}

// acquire inserts a pending reservation for ref. Kind-agnostic: the resource_kind
// is a parameter and the target column is routed by it in SQL (no per-kind Go
// branching). ON CONFLICT DO NOTHING catches EITHER per-kind partial unique index,
// so a lost race on a bin or a slot both surface as ErrReservationConflict.
//
// The bare ON CONFLICT DO NOTHING (no conflict target) is LOAD-BEARING: only
// uq_reservations_{bin,slot}_active can fire here today, so a 0-rows result means
// "reserved by someone active". A future author adding any OTHER unique constraint
// to this table would have its violations silently folded into a false
// ErrReservationConflict — handle such a constraint's conflict deliberately.
func acquire(db Execer, orderID int64, ref Ref, reservedBy string) error {
	result, err := db.Exec(
		`INSERT INTO reservations (order_id, resource_kind, bin_id, node_id, state, reserved_by)
		 VALUES ($1, $2,
		   CASE WHEN $2 = 'bin' THEN $3::bigint END,
		   CASE WHEN $2 <> 'bin' THEN $3::bigint END,
		   'pending', $4)
		 ON CONFLICT DO NOTHING`,
		orderID, string(ref.Kind), ref.ID, reservedBy,
	)
	if err != nil {
		return fmt.Errorf("reservations acquire: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		// ON CONFLICT DO NOTHING suppressed the insert — another order already
		// holds an active reservation on this resource via its per-kind index.
		return ErrReservationConflict
	}
	return nil
}

// Confirm transitions the (orderID, binID) bin reservation from "pending" to
// "confirmed", recording that the physical bin claim succeeded. A no-op if the
// row is already confirmed (idempotent for retry safety).
func Confirm(db Execer, orderID, binID int64) error {
	return confirm(db, orderID, BinRef(binID))
}

// ConfirmSlot is Confirm for a slot reservation (pending → confirmed on nodeID).
func ConfirmSlot(db Execer, orderID, nodeID int64) error {
	return confirm(db, orderID, SlotRef(nodeID))
}

// confirm flips ref's pending row to confirmed. resource_kind=$2 scopes the match
// to one kind, so COALESCE(bin_id, node_id)=$3 reads the correct target column
// (the other is NULL for that kind) — kind-agnostic, no branching.
func confirm(db Execer, orderID int64, ref Ref) error {
	_, err := db.Exec(
		`UPDATE reservations SET state='confirmed'
		 WHERE order_id=$1 AND resource_kind=$2 AND state='pending'
		   AND COALESCE(bin_id, node_id)=$3`,
		orderID, string(ref.Kind), ref.ID,
	)
	if err != nil {
		return fmt.Errorf("reservations confirm: %w", err)
	}
	return nil
}

// Release deletes the (orderID, binID) bin reservation, freeing the bin for future
// reservations. Safe to call even when no row exists (idempotent).
func Release(db Execer, orderID, binID int64) error {
	return release(db, orderID, BinRef(binID))
}

// ReleaseSlot is Release for a slot reservation (deletes the order's row on nodeID).
func ReleaseSlot(db Execer, orderID, nodeID int64) error {
	return release(db, orderID, SlotRef(nodeID))
}

// release deletes ref's row for the order. Same kind-scoped COALESCE match as confirm.
func release(db Execer, orderID int64, ref Ref) error {
	_, err := db.Exec(
		`DELETE FROM reservations
		 WHERE order_id=$1 AND resource_kind=$2 AND COALESCE(bin_id, node_id)=$3`,
		orderID, string(ref.Kind), ref.ID,
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

// ReleaseByNode deletes any reservation on nodeID — the slot dual of ReleaseByBin.
// Called at the delivered moment (commit 4) in the same tx that clears the slot's
// nodes.claimed_by, so a slot's reservation lives exactly as long as its hard
// claim. Node-keyed because the arrival path is node-centric, and the
// uq_reservations_slot_active index guarantees at most one active slot row per node.
func ReleaseByNode(db Execer, nodeID int64) error {
	_, err := db.Exec(`DELETE FROM reservations WHERE node_id=$1`, nodeID)
	if err != nil {
		return fmt.Errorf("reservations release-by-node: %w", err)
	}
	return nil
}

// ListByOrder returns all reservations held by orderID — both kinds, both states.
// The 1c plan-time reconcile loads its own holds with this BEFORE deciding what to
// keep / release / acquire — the owner-aware step that dodges the per-resource
// unique-index self-conflict documented on Acquire. Kind-threaded: each row carries
// its Kind, and exactly one of BinID/NodeID is set (the other is 0) so the reconcile
// can match a slot row by node and a bin row by bin.
func ListByOrder(db Queryer, orderID int64) ([]Reservation, error) {
	rows, err := db.Query(
		`SELECT resource_kind, bin_id, node_id, state FROM reservations WHERE order_id=$1 ORDER BY id`,
		orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("reservations list-by-order: %w", err)
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		var binID, nodeID sql.NullInt64
		if err := rows.Scan(&r.Kind, &binID, &nodeID, &r.State); err != nil {
			return nil, fmt.Errorf("reservations list-by-order scan: %w", err)
		}
		r.BinID = binID.Int64   // 0 when NULL (slot/mouth rows)
		r.NodeID = nodeID.Int64 // 0 when NULL (bin rows)
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
