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

	// Order lifecycle for AutoConfirmStuckDeliveredOrders. Raw Query is
	// exposed because the "find stale delivered" SELECT lives inline
	// in the service body — same pattern as InventoryQueryStore.
	Query(query string, args ...any) (*sql.Rows, error)
	GetOrder(id int64) (*orders.Order, error)
	UpdateOrderStatus(id int64, status, detail string) error
	CompleteOrder(id int64) error
}

// Compile-time check.
var _ ReconciliationStore = (*store.DB)(nil)
