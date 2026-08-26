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

// NewReconciliationService builds the service over an arbitrary DB.
//
// Exported for the www handler tests, which need a real service rather than the
// nil their engine stub used to return. That nil is why apiReplayOutbox had no
// coverage: any test that called it panicked on the first method, so the
// handler shipped untested and the expired-replay defect went unnoticed.
func NewReconciliationService(db *store.DB) *ReconciliationService {
	return newReconciliationService(db)
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

// GetOutboxMessage returns one outbox row so a caller can inspect the stored
// envelope before acting on it.
func (s *ReconciliationService) GetOutboxMessage(id int64) (*messaging.Message, error) {
	return s.db.GetOutboxMessage(id)
}

func (s *ReconciliationService) RequeueOutbox(id int64) error {
	return s.db.RequeueOutbox(id)
}
