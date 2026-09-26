package store

// Phase 5 delegate file: reconciliation lives in store/reconciliation/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"shingocore/store/reconciliation"
	"shingocore/store/reservations"
)

func (db *DB) ListOrderCompletionAnomalies() ([]*reconciliation.CompletionAnomaly, error) {
	return reconciliation.ListOrderCompletionAnomalies(db.DB)
}

func (db *DB) ListReconciliationAnomalies() ([]*reconciliation.Anomaly, error) {
	return reconciliation.ListAnomalies(db.DB)
}

func (db *DB) GetReconciliationSummary() (*reconciliation.Summary, error) {
	return reconciliation.GetSummary(db.DB)
}

func (db *DB) ReleaseOrphanedClaims() (int, error) {
	return reconciliation.ReleaseOrphanedClaims(db.DB)
}

// ReleaseAcquiringOrphanClaims is the same backstop's live-order arm — a claim
// held by a queued/sourcing order with no reservation behind it. See
// reconciliation.ReleaseAcquiringOrphanClaims for why it is a separate sweep
// with a separate count.
func (db *DB) ReleaseAcquiringOrphanClaims() (int, error) {
	return reconciliation.ReleaseAcquiringOrphanClaims(db.DB)
}

// ReapOrphanedReservations reaps reservation rows whose owning order is terminal
// or gone (owner-liveness, never age) — the replacement for the retired age-based Expire.
func (db *DB) ReapOrphanedReservations() (int, error) {
	return reservations.ReapOrphaned(db.DB)
}

// ListReservationsByOrder returns the order's held reservations (pending +
// confirmed) — the read the plan-time reconcile uses to recognize its own
// holds before deciding keep / release / acquire.
func (db *DB) ListReservationsByOrder(orderID int64) ([]reservations.Reservation, error) {
	return reservations.ListByOrder(db.DB, orderID)
}

// BinsReservedByOrder is the bins an order holds by reservation: the same read
// as ListReservationsByOrder, narrowed to bin rows, for a reader that may not
// name the reservations package (the planner's slot book, plan.BinsAtStep).
func (db *DB) BinsReservedByOrder(orderID int64) ([]int64, error) {
	rows, err := reservations.ListByOrder(db.DB, orderID)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, r := range rows {
		if r.Kind == reservations.KindBin {
			out = append(out, r.BinID)
		}
	}
	return out, nil
}

// ReleaseReservation deletes a single pending-only reservation for (orderID, binID)
// — used by the reserve/reconcile to drop a stray hold that no longer matches a
// need when no claim is involved. When the hold was confirmed (a claim exists),
// use the coupled ReleaseClaimForBin instead so claim + reservation go together.
func (db *DB) ReleaseReservation(orderID, binID int64) error {
	return reservations.Release(db.DB, orderID, binID)
}
