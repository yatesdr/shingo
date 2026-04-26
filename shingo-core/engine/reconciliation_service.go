package engine

import (
	"fmt"
	"time"

	"shingocore/store"
	"shingocore/store/messaging"
	"shingocore/store/reconciliation"
	"shingocore/store/recovery"
)

type ReconciliationService struct {
	db               *store.DB
	logFn            LogFunc
	onOrderCompleted func(orderID int64, edgeUUID, stationID string) // called after auto-confirm to trigger bin movement
}

func newReconciliationService(db *store.DB, logFn LogFunc) *ReconciliationService {
	return &ReconciliationService{db: db, logFn: logFn}
}

func (s *ReconciliationService) Summary() (*reconciliation.Summary, error) {
	return s.db.GetReconciliationSummary()
}

func (s *ReconciliationService) ListAnomalies() ([]*reconciliation.Anomaly, error) {
	return s.db.ListReconciliationAnomalies()
}

func (s *ReconciliationService) ListRecoveryActions(limit int) ([]*recovery.Action, error) {
	return s.db.ListRecoveryActions(limit)
}

func (s *ReconciliationService) RequeueOutbox(id int64) error {
	return s.db.RequeueOutbox(id)
}

func (s *ReconciliationService) ListDeadLetterOutbox(limit int) ([]*messaging.OutboxMessage, error) {
	return s.db.ListDeadLetterOutbox(limit)
}

func (s *ReconciliationService) Loop(stopCh <-chan struct{}, interval, autoConfirmTimeout time.Duration) {
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
			if autoConfirmTimeout > 0 {
				if n, err := s.AutoConfirmStuckDeliveredOrders(autoConfirmTimeout); err != nil {
					s.logFn("engine: auto-confirm delivered error: %v", err)
				} else if n > 0 {
					s.logFn("engine: auto-confirmed %d stuck delivered orders", n)
				}
			}
		}
	}
}

// AutoConfirmStuckDeliveredOrders confirms delivered orders that have been
// waiting longer than the configured timeout. Returns count of auto-confirmed orders.
func (s *ReconciliationService) AutoConfirmStuckDeliveredOrders(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		return 0, nil
	}

	rows, err := s.db.Query(`
		SELECT id
		FROM orders
		WHERE status = 'delivered'
		  AND completed_at IS NULL
		  AND updated_at < NOW() - ($1 * INTERVAL '1 second')
		ORDER BY updated_at ASC
		LIMIT 100`, int(timeout.Seconds()))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var orderIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		orderIDs = append(orderIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	confirmed := 0
	for _, id := range orderIDs {
		order, err := s.db.GetOrder(id)
		if err != nil || order.Status != "delivered" {
			continue
		}
		detail := fmt.Sprintf("auto-confirmed after %s timeout", timeout)
		if err := s.db.UpdateOrderStatus(order.ID, "confirmed", detail); err != nil {
			s.logFn("engine: auto-confirm order %d status: %v", order.ID, err)
			continue
		}
		if err := s.db.CompleteOrder(order.ID); err != nil {
			s.logFn("engine: complete auto-confirmed order %d: %v", order.ID, err)
			continue
		}
		s.logFn("engine: auto-confirmed stuck delivered order %d (uuid=%s)", order.ID, order.EdgeUUID)
		s.db.RecordRecoveryAction("auto_confirm_delivered", "order", order.ID,
			fmt.Sprintf("auto-confirmed delivered order after %s timeout", timeout), "system")
		// Trigger bin movement — without this, ApplyBinArrival never runs
		// and the bin stays at its source node instead of moving to the destination.
		if s.onOrderCompleted != nil {
			s.onOrderCompleted(order.ID, order.EdgeUUID, order.StationID)
		}
		confirmed++
	}

	return confirmed, nil
}
