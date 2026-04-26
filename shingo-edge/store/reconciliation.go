package store

// Phase 5b delegate file: edge reconciliation queries now live in
// store/reconciliation/. This file preserves the *store.DB method
// surface so external callers do not need to change.

import "shingoedge/store/reconciliation"

// ListReconciliationAnomalies returns anomalies for stuck active
// orders and for delivered-but-unconfirmed orders.
func (db *DB) ListReconciliationAnomalies() ([]*reconciliation.Anomaly, error) {
	return reconciliation.ListAnomalies(db.DB)
}

// GetReconciliationSummary returns a Summary with anomaly + outbox
// counts and a derived overall status.
func (db *DB) GetReconciliationSummary() (*reconciliation.Summary, error) {
	return reconciliation.GetSummary(db.DB)
}
