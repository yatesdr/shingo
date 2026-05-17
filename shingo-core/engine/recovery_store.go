package engine

import (
	"time"

	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// RecoveryStore is the narrow DB surface RecoveryService depends on.
//
// RecoveryService is engine-orchestration-heavy (it reaches into
// e.Events, e.dispatcher, e.fleet, e.TerminateOrder, e.isStorageSlot,
// e.resolveStagingExpiry as well as e.db). Extracting RecoveryStore
// makes the DB dependency explicit without forcing a structural split
// of the engine-coupling. RecoveryService gains a `db RecoveryStore`
// field alongside its existing `engine *Engine`; the wiring stays
// minimal.
type RecoveryStore interface {
	// Order / node / bin lookups used by every recovery action.
	GetOrder(id int64) (*orders.Order, error)
	GetNodeByDotName(name string) (*nodes.Node, error)
	GetBin(id int64) (*bins.Bin, error)

	// Recovery-specific mutations.
	RepairConfirmedOrderCompletion(orderID, binID, toNodeID int64, staged bool, expiresAt *time.Time) error
	ReleaseTerminalBinClaim(binID int64) (int64, error)
	ReleaseStagedBin(binID int64) error

	// Audit + recovery action log writes.
	AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error
	RecordRecoveryAction(action, targetType string, targetID int64, detail, actor string) error
}

// Compile-time check.
var _ RecoveryStore = (*store.DB)(nil)
