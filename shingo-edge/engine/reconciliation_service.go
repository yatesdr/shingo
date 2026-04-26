package engine

import (
	"shingoedge/store"
	"shingoedge/store/messaging"
	"shingoedge/store/reconciliation"
)

type ReconciliationService struct {
	db *store.DB
}

func newReconciliationService(db *store.DB) *ReconciliationService {
	return &ReconciliationService{db: db}
}

func (s *ReconciliationService) Summary() (*reconciliation.Summary, error) {
	return s.db.GetReconciliationSummary()
}

func (s *ReconciliationService) ListAnomalies() ([]*reconciliation.Anomaly, error) {
	return s.db.ListReconciliationAnomalies()
}

func (s *ReconciliationService) ListDeadLetterOutbox(limit int) ([]messaging.Message, error) {
	return s.db.ListDeadLetterOutbox(limit)
}

func (s *ReconciliationService) RequeueOutbox(id int64) error {
	return s.db.RequeueOutbox(id)
}
