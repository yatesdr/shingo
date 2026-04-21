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
	// ── Subsystem accessors ────────────────────────────────────────
	CoreAPI() *engine.CoreClient
	AppConfig() *config.Config
	ConfigPath() string
	PLCManager() *plc.Manager
	OrderManager() *orders.Manager
	Reconciliation() *engine.ReconciliationService
	CoreSync() *engine.CoreSyncService

	// ── Engine lifecycle / messaging ───────────────────────────────
	ApplyWarLinkConfig()
	ReconnectKafka() error
	SendEnvelope(env *protocol.Envelope) error
	CoreNodes() map[string]protocol.NodeInfo
	RequestNodeSync()
	RequestCatalogSync()
	RequestOrderStatusSync() error
	StartupReconcile() error

	// ── Material orchestration ─────────────────────────────────────
	RequestNodeMaterial(nodeID int64, quantity int64) (*engine.NodeOrderResult, error)
	ReleaseNodeEmpty(nodeID int64) (*store.Order, error)
	ReleaseNodePartial(nodeID int64, qty int64) (*store.Order, error)
	ReleaseStagedOrders(nodeID int64) error
	ConfirmNodeManifest(nodeID int64) error
	FinalizeProduceNode(nodeID int64) (*engine.NodeOrderResult, error)
	LoadBin(nodeID int64, payloadCode string, uopCount int64, manifest []protocol.IngestManifestItem) error
	ClearBin(nodeID int64) error
	RequestEmptyBin(nodeID int64, payloadCode string) (*store.Order, error)
	RequestFullBin(nodeID int64, payloadCode string) (*store.Order, error)

	// ── Changeover orchestration ───────────────────────────────────
	StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*store.ProcessChangeover, error)
	CompleteProcessProductionCutover(processID int64) error
	CancelProcessChangeover(processID int64) error
	CancelProcessChangeoverRedirect(processID int64, nextStyleID *int64) error
	ReleaseChangeoverWait(processID int64) error
	StageNodeChangeoverMaterial(processID, nodeID int64) (*store.Order, error)
	EmptyNodeForToolChange(processID, nodeID int64, partialQty int64) (*store.Order, error)
	ReleaseNodeIntoProduction(processID, nodeID int64) (*store.Order, error)
	SwitchNodeToTarget(processID, nodeID int64) error
	SwitchOperatorStationToTarget(processID, stationID int64) error
	SyncProcessCounter(processID int64) error
	FlipABNode(nodeID int64) error

	// ── WarLink tag management ─────────────────────────────────────
	EnsureTagPublished(rpID int64, plcName, tagName string)
	ManageReportingPointTag(rpID int64, oldPLC, oldTag string, oldManaged bool, newPLC, newTag string)
	CleanupReportingPointTag(rpID int64, plcName, tagName string, managed bool)

	// ── Named DB methods (Phase 4) ─────────────────────────────────
	// Each method below corresponds 1:1 with a *store.DB method that
	// www handlers used to reach via the DB() escape hatch. See
	// shingo-edge/engine/engine_db_methods.go for the delegations and
	// docs/edge-named-methods.md for the per-call-site traceability table.
	AdminUserExists() (bool, error)
	BuildOperatorStationView(stationID int64) (*store.OperatorStationView, error)
	ConfirmAnomaly(id int64) error
	CreateAdminUser(username, passwordHash string) (int64, error)
	CreateOperatorStation(in store.OperatorStationInput) (int64, error)
	CreateProcess(name, description, productionState, counterPLC, counterTag string, counterEnabled bool) (int64, error)
	CreateProcessNode(in store.ProcessNodeInput) (int64, error)
	CreateReportingPoint(plcName, tagName string, styleID int64) (int64, error)
	CreateStyle(name, description string, processID int64) (int64, error)
	DeleteOperatorStation(id int64) error
	DeleteProcess(id int64) error
	DeleteProcessNode(id int64) error
	DeleteReportingPoint(id int64) error
	DeleteShift(shiftNumber int) error
	DeleteStyle(id int64) error
	DeleteStyleNodeClaim(id int64) error
	DismissAnomaly(id int64) error
	EnsureProcessNodeRuntime(processNodeID int64) (*store.ProcessNodeRuntimeState, error)
	GetActiveProcessChangeover(processID int64) (*store.ProcessChangeover, error)
	GetAdminUser(username string) (*store.AdminUser, error)
	GetOperatorStation(id int64) (*store.OperatorStation, error)
	GetOrder(id int64) (*store.Order, error)
	GetProcessNode(id int64) (*store.ProcessNode, error)
	GetReportingPoint(id int64) (*store.ReportingPoint, error)
	GetStationNodeNames(stationID int64) ([]string, error)
	GetStyle(id int64) (*store.Style, error)
	GetStyleNodeClaim(id int64) (*store.StyleNodeClaim, error)
	HourlyCountTotals(processID int64, countDate string) (map[int]int64, error)
	ListActiveOrders() ([]store.Order, error)
	ListActiveOrdersByProcess(processID int64) ([]store.Order, error)
	ListChangeoverNodeTasks(changeoverID int64) ([]store.ChangeoverNodeTask, error)
	ListChangeoverStationTasks(changeoverID int64) ([]store.ChangeoverStationTask, error)
	ListHourlyCounts(processID, styleID int64, countDate string) ([]store.HourlyCount, error)
	ListOperatorStations() ([]store.OperatorStation, error)
	ListOperatorStationsByProcess(processID int64) ([]store.OperatorStation, error)
	ListPayloadCatalog() ([]*store.PayloadCatalogEntry, error)
	ListProcessChangeovers(processID int64) ([]store.ProcessChangeover, error)
	ListProcessNodes() ([]store.ProcessNode, error)
	ListProcessNodesByProcess(processID int64) ([]store.ProcessNode, error)
	ListProcessNodesByStation(stationID int64) ([]store.ProcessNode, error)
	ListProcesses() ([]store.Process, error)
	ListReportingPoints() ([]store.ReportingPoint, error)
	ListShifts() ([]store.Shift, error)
	ListStyleNodeClaims(styleID int64) ([]store.StyleNodeClaim, error)
	ListStyles() ([]store.Style, error)
	ListStylesByProcess(processID int64) ([]store.Style, error)
	ListUnconfirmedAnomalies() ([]store.CounterSnapshot, error)
	MoveOperatorStation(id int64, direction string) error
	SetActiveStyle(processID int64, styleID *int64) error
	SetStationNodes(stationID int64, nodeNames []string) error
	TouchOperatorStation(id int64, healthStatus string) error
	UpdateAdminPassword(username, passwordHash string) error
	UpdateOperatorStation(id int64, in store.OperatorStationInput) error
	UpdateOrderFinalCount(id int64, finalCount int64, confirmed bool) error
	UpdateProcess(id int64, name, description, productionState, counterPLC, counterTag string, counterEnabled bool) error
	UpdateProcessNode(id int64, in store.ProcessNodeInput) error
	UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error
	UpdateReportingPoint(id int64, plcName, tagName string, styleID int64, enabled bool) error
	UpdateStyle(id int64, name, description string, processID int64) error
	UpsertShift(shiftNumber int, name, startTime, endTime string) error
	UpsertStyleNodeClaim(in store.StyleNodeClaimInput) (int64, error)
}

// Compile-time assertion: *engine.Engine must satisfy EngineAccess.
var _ EngineAccess = (*engine.Engine)(nil)
