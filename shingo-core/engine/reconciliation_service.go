package engine

import (
	"fmt"
	"time"

	"shingocore/store/messaging"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
	"shingocore/store/recovery"
)

// ReconciliationService runs the periodic reconciliation loop plus
// auto-confirm-stuck-delivered logic. db is declared as the
// ReconciliationStore interface (see reconciliation_store.go); *store.DB
// satisfies it structurally so engine wiring is unchanged.
//
// confirmDelivered is a late-bound callback the engine wires up after
// the dispatcher is constructed (engine.New → engine.Start ordering).
// AutoConfirmStuckDeliveredOrders calls it once per stuck order; the
// production binding routes through dispatch.LifecycleService.ConfirmReceipt
// so the (Delivered → Confirmed) actionMap entry fires fireCompleted →
// EmitOrderCompleted. The old direct-DB path bypassed that emit, which
// left Edge stranded at delivered.
type ReconciliationService struct {
	db               ReconciliationStore
	logFn            LogFunc
	confirmDelivered func(order *orders.Order) error
	// abandonOrder cancels a stuck order (and cascades to its two-robot
	// sibling). Late-bound to dispatch.LifecycleService.CancelOrder in
	// engine.New, same wiring rationale as confirmDelivered above.
	abandonOrder func(order *orders.Order, reason string) error
}

func newReconciliationService(db ReconciliationStore, logFn LogFunc) *ReconciliationService {
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

func (s *ReconciliationService) Loop(stopCh <-chan struct{}, interval, autoConfirmTimeout, abandonTimeout time.Duration) {
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
			if abandonTimeout > 0 {
				if n, err := s.AbandonStuckOrders(abandonTimeout); err != nil {
					s.logFn("engine: abandon stuck orders error: %v", err)
				} else if n > 0 {
					s.logFn("engine: abandoned %d stuck orders", n)
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
		  AND NOT skip_auto_confirm
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

	if s.confirmDelivered == nil {
		// Unwired callback — never the production path (engine.New sets it),
		// but bare unit fixtures may construct the service without one.
		// Log + no-op rather than panic so the periodic Loop survives.
		s.logFn("engine: auto-confirm skipped (%d candidate orders): confirmDelivered callback not wired", len(orderIDs))
		return 0, nil
	}

	confirmed := 0
	for _, id := range orderIDs {
		order, err := s.db.GetOrder(id)
		if err != nil {
			s.logFn("engine: auto-confirm reload order %d: %v (skipping this pass; periodic loop retries)", id, err)
			continue
		}
		if order.Status != "delivered" {
			continue // no longer delivered — nothing to confirm
		}
		if err := s.confirmDelivered(order); err != nil {
			s.logFn("engine: auto-confirm order %d: %v", order.ID, err)
			continue
		}
		s.logFn("engine: auto-confirmed stuck delivered order %d (uuid=%s)", order.ID, order.EdgeUUID)
		s.db.RecordRecoveryAction("auto_confirm_delivered", "order", order.ID,
			fmt.Sprintf("auto-confirmed delivered order after %s timeout", timeout), "system")
		confirmed++
	}

	return confirmed, nil
}

// AbandonStuckOrders cancels non-terminal orders that have sat without
// progress past the timeout — a held two-robot swap removal leg whose
// supply never arrives, or a robot parked at a staging node. Without it a
// stuck swap ties up a robot and clutters the board until an operator
// intervenes (ALN_003 swap-starvation, 2026-06-03). Cancelling reuses the
// standard teardown (fleet cancel, bin unclaim, auto-return, Edge notify)
// and cascades to the swap sibling. Returns the count abandoned.
func (s *ReconciliationService) AbandonStuckOrders(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		return 0, nil
	}

	rows, err := s.db.Query(`
		SELECT id
		FROM orders
		WHERE status IN ('queued', 'staged')
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

	if s.abandonOrder == nil {
		s.logFn("engine: abandon-stuck skipped (%d candidate orders): abandonOrder callback not wired", len(orderIDs))
		return 0, nil
	}

	abandoned := 0
	for _, id := range orderIDs {
		order, err := s.db.GetOrder(id)
		if err != nil {
			s.logFn("engine: abandon-stuck reload order %d: %v (skipping this pass; periodic loop retries)", id, err)
			continue
		}
		// A sibling cancel from an earlier iteration this pass may already
		// have moved this one terminal — skip if no longer queued/staged.
		if order.Status != "queued" && order.Status != "staged" {
			continue
		}
		reason := fmt.Sprintf("abandoned: stuck in %s past %s", order.Status, timeout)
		if err := s.abandonOrder(order, reason); err != nil {
			s.logFn("engine: abandon stuck order %d: %v", order.ID, err)
			continue
		}
		s.logFn("engine: abandoned stuck order %d (uuid=%s status=%s)", order.ID, order.EdgeUUID, order.Status)
		s.db.RecordRecoveryAction("abandon_stuck_order", "order", order.ID, reason, "system")
		abandoned++
	}

	return abandoned, nil
}
