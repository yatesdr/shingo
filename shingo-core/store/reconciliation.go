package store

// Phase 5 delegate file: reconciliation lives in store/reconciliation/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"shingocore/store/reconciliation"
)

// OrderCompletionAnomaly preserves the store.OrderCompletionAnomaly public API.
type OrderCompletionAnomaly = reconciliation.CompletionAnomaly

// ReconciliationAnomaly preserves the store.ReconciliationAnomaly public API.
type ReconciliationAnomaly = reconciliation.Anomaly

// ReconciliationSummary preserves the store.ReconciliationSummary public API.
type ReconciliationSummary = reconciliation.Summary

func (db *DB) ListOrderCompletionAnomalies() ([]*OrderCompletionAnomaly, error) {
	return reconciliation.ListOrderCompletionAnomalies(db.DB)
}

func (db *DB) ListReconciliationAnomalies() ([]*ReconciliationAnomaly, error) {
	return reconciliation.ListAnomalies(db.DB)
}

func (db *DB) GetReconciliationSummary() (*ReconciliationSummary, error) {
	return reconciliation.GetSummary(db.DB)
}

func (db *DB) ReleaseOrphanedClaims() (int, error) {
	return reconciliation.ReleaseOrphanedClaims(db.DB)
}
