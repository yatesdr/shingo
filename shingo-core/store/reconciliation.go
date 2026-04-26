package store

// Phase 5 delegate file: reconciliation lives in store/reconciliation/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import "shingocore/store/reconciliation"

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
