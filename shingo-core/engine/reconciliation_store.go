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

	// DigStillOwesItsTarget says whether a reshuffle parent that LOOKS finished
	// — sealed, in `reshuffling`, every child terminal — is in fact holding its
	// lane until the bin it dug out is collected. AdvanceStuckReshuffleParents
	// asks it so the sweep does not "rescue" a dig that is working correctly and
	// file a recovery record for it.
	//
	// It is the store's method rather than a callback because dispatch asks the
	// same question at two more sites, and the three must not be able to
	// disagree about what the hold means.
	DigStillOwesItsTarget(parent *orders.Order) (bool, error)

	// ReapOrphanedReservations reaps reservation rows (pending AND confirmed) whose
	// owning order is terminal or gone — the owner-liveness backstop behind
	// TerminalizeOrder. Never age-based: a hold under a live order is sacred. Returns
	// the count of rows deleted.
	ReapOrphanedReservations() (int, error)
}

// Compile-time check.
var _ ReconciliationStore = (*store.DB)(nil)
