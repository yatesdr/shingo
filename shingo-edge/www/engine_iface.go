package www

import (
	"shingoedge/changeover"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/store"

	"shingo/protocol"
)

// EngineAccess defines the interface that www handlers require from the engine.
// Defined here (consumer side) per Go convention to enable test doubles.
// The concrete *engine.Engine satisfies this interface.
type EngineAccess interface {
	DB() *store.DB
	AppConfig() *config.Config
	ConfigPath() string
	PLCManager() *plc.Manager
	OrderManager() *orders.Manager
	Reconciliation() *engine.ReconciliationService
	CoreSync() *engine.CoreSyncService
	ChangeoverMachine(lineID int64) *changeover.Machine
	ChangeoverMachines() map[int64]*changeover.Machine
	ApplyWarLinkConfig()
	ReconnectKafka() error
	SendEnvelope(env *protocol.Envelope) error
	CoreNodes() map[string]protocol.NodeInfo
	RequestNodeSync()
	RequestCatalogSync()
	RequestOrderStatusSync() error
	StartupReconcile() error
	RequestOrders(payloadID int64, quantity int64) (*engine.OrderRequestResult, error)

	// WarLink tag management
	EnsureTagPublished(rpID int64, plcName, tagName string)
	ManageReportingPointTag(rpID int64, oldPLC, oldTag string, oldManaged bool, newPLC, newTag string)
	CleanupReportingPointTag(rpID int64, plcName, tagName string, managed bool)
}
