package engine

import (
	"time"

	"shingocore/store"
)

type ReconciliationService struct {
	db    *store.DB
	logFn LogFunc
}

func newReconciliationService(db *store.DB, logFn LogFunc) *ReconciliationService {
	return &ReconciliationService{db: db, logFn: logFn}
}

func (s *ReconciliationService) Summary() (*store.ReconciliationSummary, error) {
	return s.db.GetReconciliationSummary()
}

func (s *ReconciliationService) ListAnomalies() ([]*store.ReconciliationAnomaly, error) {
	return s.db.ListReconciliationAnomalies()
}

func (s *ReconciliationService) ListRecoveryActions(limit int) ([]*store.RecoveryAction, error) {
	return s.db.ListRecoveryActions(limit)
}

func (s *ReconciliationService) RequeueOutbox(id int64) error {
	return s.db.RequeueOutbox(id)
}

func (s *ReconciliationService) ListDeadLetterOutbox(limit int) ([]*store.OutboxMessage, error) {
	return s.db.ListDeadLetterOutbox(limit)
}

func (s *ReconciliationService) Loop(stopCh <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			summary, err := s.Summary()
			if err != nil {
				s.logFn("engine: reconciliation summary error: %v", err)
				continue
			}
			if summary.Status != "ok" {
				s.logFn("engine: reconciliation status=%s anomalies=%d stuck=%d staged=%d stale_edges=%d outbox=%d dead_letters=%d",
					summary.Status,
					summary.TotalAnomalies,
					summary.StuckOrders,
					summary.ExpiredStagedBins,
					summary.StaleEdges,
					summary.OutboxPending,
					summary.DeadLetters,
				)
			}
		}
	}
}
