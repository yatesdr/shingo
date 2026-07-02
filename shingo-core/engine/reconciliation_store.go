package engine

import (
	"database/sql"

	"shingocore/store"
	"shingocore/store/messaging"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
	"shingocore/store/recovery"
)

// ReconciliationStore is the narrow DB surface ReconciliationService
// depends on.
//
// Pattern matches dispatch/capacity.go, fulfillment/store.go,
// material/store.go, dispatch/binresolver/store.go — declare the
// interface consumer-side, *store.DB satisfies it structurally, no
// engine wiring change.
type ReconciliationStore interface {
	// Reconciliation rollups.
	GetReconciliationSummary() (*reconciliation.Summary, error)
	ListReconciliationAnomalies() ([]*reconciliation.Anomaly, error)

	// Recovery action log.
	ListRecoveryActions(limit int) ([]*recovery.Action, error)
	RecordRecoveryAction(action, targetType string, targetID int64, detail, actor string) error

	// Outbox introspection / replay.
	RequeueOutbox(id int64) error
	ListDeadLetterOutbox(limit int) ([]*messaging.OutboxMessage, error)

	// Order lookups for AutoConfirmStuckDeliveredOrders. Raw Query is
	// exposed because the "find stale delivered" SELECT lives inline
	// in the service body — same pattern as InventoryQueryStore. Status
	// transitions are not in this surface: AutoConfirm routes through
	// the confirmDelivered callback (wired in engine.New to
	// LifecycleService.ConfirmReceipt) so the state machine emits
	// EmitOrderCompleted to Edge.
	Query(query string, args ...any) (*sql.Rows, error)
	GetOrder(id int64) (*orders.Order, error)

	// ExpireReservations deletes pending reservations whose expires_at is
	// in the past, freeing bins held by orders that crashed between Acquire
	// and Confirm. Returns the count of rows deleted.
	ExpireReservations() (int, error)
}

// Compile-time check.
var _ ReconciliationStore = (*store.DB)(nil)
