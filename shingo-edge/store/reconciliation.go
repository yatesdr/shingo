package store

// Phase 5b delegate file: edge reconciliation queries now live in
// store/reconciliation/. This file preserves the *store.DB method
// surface so external callers do not need to change.

import "shingoedge/store/reconciliation"

// ReconciliationAnomaly is one observed reconciliation issue.
type ReconciliationAnomaly = reconciliation.Anomaly

// ReconciliationSummary aggregates anomaly + outbox counts and a
// derived overall status.
type ReconciliationSummary = reconciliation.Summary

// ListReconciliationAnomalies returns anomalies for stuck active
// orders and for delivered-but-unconfirmed orders.
func (db *DB) ListReconciliationAnomalies() ([]*ReconciliationAnomaly, error) {
	return reconciliation.ListAnomalies(db.DB)
}

// GetReconciliationSummary returns a Summary with anomaly + outbox
// counts and a derived overall status.
func (db *DB) GetReconciliationSummary() (*ReconciliationSummary, error) {
	return reconciliation.GetSummary(db.DB)
}
