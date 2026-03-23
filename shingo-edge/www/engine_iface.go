package www

import (
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
	ApplyWarLinkConfig()
	ReconnectKafka() error
	SendEnvelope(env *protocol.Envelope) error
	CoreNodes() map[string]protocol.NodeInfo
	RequestNodeSync()
	RequestCatalogSync()
	RequestOrderStatusSync() error
	StartupReconcile() error
	RequestNodeMaterial(nodeID int64, quantity int64) (*engine.NodeOrderResult, error)
	ReleaseNodeEmpty(nodeID int64) (*store.Order, error)
	ReleaseNodePartial(nodeID int64, qty int64) (*store.Order, error)
	ConfirmNodeManifest(nodeID int64) error
	FinalizeProduceNode(nodeID int64) (*store.Order, error)
	StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*store.ProcessChangeover, error)
	CompleteProcessProductionCutover(processID int64) error
	CancelProcessChangeover(processID int64) error
	StageNodeChangeoverMaterial(processID, nodeID int64) (*store.Order, error)
	EmptyNodeForToolChange(processID, nodeID int64, partialQty int64) (*store.Order, error)
	ReleaseNodeIntoProduction(processID, nodeID int64) (*store.Order, error)
	SwitchNodeToTarget(processID, nodeID int64) error
	SwitchOperatorStationToTarget(processID, stationID int64) error
	SyncProcessCounter(processID int64) error

	// WarLink tag management
	EnsureTagPublished(rpID int64, plcName, tagName string)
	ManageReportingPointTag(rpID int64, oldPLC, oldTag string, oldManaged bool, newPLC, newTag string)
	CleanupReportingPointTag(rpID int64, plcName, tagName string, managed bool)
}
