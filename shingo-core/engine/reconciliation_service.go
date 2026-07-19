package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
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
	// advanceCompound re-drives a compound (reshuffle) parent whose children
	// are all terminal. Late-bound to dispatch.Dispatcher.AdvanceCompoundOrder
	// in engine.New. The liveness backstop for reshuffle parents stranded in
	// `reshuffling` when a child→parent terminal event was missed (crash) or
	// never fired (the cancelled-child vector has no child→parent event arm).
	advanceCompound func(parentID int64) error
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
			if n, err := s.AdvanceStuckReshuffleParents(); err != nil {
				s.logFn("engine: advance stuck reshuffle parents error: %v", err)
			} else if n > 0 {
				s.logFn("engine: re-drove %d stuck reshuffle parents", n)
			}
			if n, err := s.db.ReapOrphanedReservations(); err != nil {
				s.logFn("engine: reap orphaned reservations error: %v", err)
			} else if n > 0 {
				s.logFn("engine: reaped %d orphaned reservations from terminal/gone orders", n)
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

	// Compare against the injectable clock, NOT the DB's wall NOW(): order
	// updated_at is stamped with clock.Now() (sim-time in sim — orders/orders.go),
	// so a wall-NOW() comparison never fires once the sim clock outruns wall time
	// (10× → immediately), silently stranding every delivery at 'delivered'. In
	// production clock.Now() == time.Now(), so behaviour is unchanged there.
	cutoff := clock.Now().UTC().Add(-timeout)
	rows, err := s.db.Query(`
		SELECT id
		FROM orders
		WHERE status = 'delivered'
		  AND completed_at IS NULL
		  AND updated_at < $1
		  AND NOT skip_auto_confirm
		ORDER BY updated_at ASC
		LIMIT 100`, cutoff)
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

// AdvanceStuckReshuffleParents is the liveness backstop for compound (reshuffle)
// parents left in `reshuffling` after ALL their children reached a terminal status —
// a state that should be transient (a child→parent terminal event re-drives the
// parent) but strands FOREVER when that event is missed: a Core crash between the
// last child's terminal transition and AdvanceCompoundOrder, or the cancelled-child
// vector, which has no child→parent event arm at all. For each such parent it
// re-drives AdvanceCompoundOrder, which resumes (coordinated) / completes (plain) /
// fails (a failed-or-cancelled child) the parent per the children's terminal states.
// Idempotent: a parent advanced out of `reshuffling` is not re-selected next pass.
func (s *ReconciliationService) AdvanceStuckReshuffleParents() (int, error) {
	rows, err := s.db.Query(`
		SELECT p.id
		FROM orders p
		WHERE p.status = 'reshuffling'
		  AND EXISTS (SELECT 1 FROM orders c WHERE c.parent_order_id = p.id)
		  AND NOT EXISTS (
			SELECT 1 FROM orders c
			WHERE c.parent_order_id = p.id
			  AND c.status NOT IN ('confirmed', 'failed', 'cancelled', 'skipped')
		  )
		ORDER BY p.id
		LIMIT 100`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var parentIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		parentIDs = append(parentIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if s.advanceCompound == nil {
		// Unwired callback — never the production path (engine.New sets it), but bare
		// unit fixtures may omit it. Log + no-op rather than panic.
		if len(parentIDs) > 0 {
			s.logFn("engine: reshuffle-liveness skipped (%d stuck parents): advanceCompound callback not wired", len(parentIDs))
		}
		return 0, nil
	}

	advanced := 0
	for _, id := range parentIDs {
		if err := s.advanceCompound(id); err != nil {
			s.logFn("engine: re-drive stuck reshuffle parent %d: %v (skipping this pass; loop retries)", id, err)
			continue
		}
		s.logFn("engine: re-drove stuck reshuffle parent %d (all children terminal)", id)
		s.db.RecordRecoveryAction("advance_stuck_reshuffle", "order", id,
			"re-drove compound parent stranded in reshuffling with all children terminal", "system")
		advanced++
	}
	return advanced, nil
}

// AbandonStuckOrders cancels RUNTIME-stuck orders that have sat without progress past the
// timeout: a robot parked at a staging node (staged), or a leg handed to the fleet that
// never started moving (dispatched). The latter is the long-weekend drain — orders
// dispatched Friday whose robots dwelled all weekend, drained, and faulted on transport
// when finally moved (2026-06-05/07) sit at `dispatched`/vendor CREATED.
//
// Scope = protocol.IsStuckSweepCandidate ({dispatched, staged}). in_transit is excluded (an
// actively moving robot is not stuck). PRE-DISPATCH WAITING (queued/sourcing) is excluded
// per the operator-driven-demand rule: demand is operator-driven and never
// evaporates, so a waiting order holds INDEFINITELY and is never abandoned on a
// timer — a wait of days is legitimate, and give-up is an operator decision. This
// is the narrowing of the old
// {queued,staged,sourcing,dispatched} set: a swap removal leg whose supply never arrives now
// WAITS (the sibling gate holds it in queued/sourcing) rather than being auto-cancelled at
// ~1h; the operator cancels if it is truly abandoned.
//
// Cancelling reuses the standard teardown (fleet cancel, bin unclaim, auto-return, Edge
// notify) and cascades to the swap sibling. Returns the count abandoned.
func (s *ReconciliationService) AbandonStuckOrders(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		return 0, nil
	}

	// Sim-clock cutoff, same rationale as AutoConfirmStuckDeliveredOrders — order
	// updated_at is clock.Now()-stamped, so a wall-NOW() comparison never fires in sim.
	cutoff := clock.Now().UTC().Add(-timeout)
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT id
		FROM orders
		WHERE status IN (%s)
		  AND updated_at < $1
		ORDER BY updated_at ASC
		LIMIT 100`, protocol.StuckSweepStatusSQLList()), cutoff)
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
		// have moved this one out of the stuck-sweep set (terminal, or a
		// re-queue back to a pre-dispatch waiting state) — skip if it is no
		// longer a runtime-stuck candidate.
		if !protocol.IsStuckSweepCandidate(order.Status) {
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
