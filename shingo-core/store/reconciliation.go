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

// ReapOrphanedReservations reaps reservation rows whose owning order is terminal or gone
// (owner-liveness, D18-Q4) — the 1c replacement for the retired age-based Expire.
func (db *DB) ReapOrphanedReservations() (int, error) {
	return reservations.ReapOrphaned(db.DB)
}

// ListReservationsByOrder returns the order's held reservations (pending +
// confirmed) — the read the 1c plan-time reconcile uses to recognize its own
// holds before deciding keep / release / acquire.
func (db *DB) ListReservationsByOrder(orderID int64) ([]reservations.Reservation, error) {
	return reservations.ListByOrder(db.DB, orderID)
}

// ReleaseReservation deletes a single pending-only reservation for (orderID, binID)
// — used by the reserve/reconcile to drop a stray hold that no longer matches a
// need when no claim is involved. When the hold was confirmed (a claim exists),
// use the coupled ReleaseClaimForBin instead so claim + reservation go together.
func (db *DB) ReleaseReservation(orderID, binID int64) error {
	return reservations.Release(db.DB, orderID, binID)
}
