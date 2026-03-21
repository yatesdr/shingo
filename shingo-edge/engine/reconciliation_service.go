package engine

import "shingoedge/store"

type ReconciliationService struct {
	db *store.DB
}

func newReconciliationService(db *store.DB) *ReconciliationService {
	return &ReconciliationService{db: db}
}

func (s *ReconciliationService) Summary() (*store.ReconciliationSummary, error) {
	return s.db.GetReconciliationSummary()
}

func (s *ReconciliationService) ListAnomalies() ([]*store.ReconciliationAnomaly, error) {
	return s.db.ListReconciliationAnomalies()
}

func (s *ReconciliationService) ListDeadLetterOutbox(limit int) ([]store.OutboxMessage, error) {
	return s.db.ListDeadLetterOutbox(limit)
}

func (s *ReconciliationService) RequeueOutbox(id int64) error {
	return s.db.RequeueOutbox(id)
}
