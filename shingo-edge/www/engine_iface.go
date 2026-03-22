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
	RequestOpNodeMaterial(opNodeID int64, quantity int64) (*engine.OpNodeOrderResult, error)
	ReleaseOpNodeEmpty(opNodeID int64) (*store.Order, error)
	ReleaseOpNodePartial(opNodeID int64, qty int64) (*store.Order, error)
	ConfirmOpNodeManifest(opNodeID int64) error
	StartProcessChangeoverV2(processID, toStyleID int64, calledBy, notes string) (*store.ProcessChangeover, error)
	AdvanceProcessChangeoverPhase(processID int64, phase string) error
	CancelProcessChangeoverV2(processID int64) error
	StageOpNodeChangeoverMaterial(processID, opNodeID int64) (*store.Order, error)
	EmptyOpNodeForToolChange(processID, opNodeID int64, partialQty int64) (*store.Order, error)
	ReleaseOpNodeIntoProduction(processID, opNodeID int64) (*store.Order, error)
	SwitchOpNodeToTarget(processID, opNodeID int64) error
	SwitchOperatorStationToTarget(processID, stationID int64) error

	// WarLink tag management
	EnsureTagPublished(rpID int64, plcName, tagName string)
	ManageReportingPointTag(rpID int64, oldPLC, oldTag string, oldManaged bool, newPLC, newTag string)
	CleanupReportingPointTag(rpID int64, plcName, tagName string, managed bool)
}
