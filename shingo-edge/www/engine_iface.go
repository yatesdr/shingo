package www

import (
	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/service"
)

// ServiceAccess is the narrow interface that service-shaped www handlers
// require from the engine: subsystem accessors + per-domain service
// accessors. CRUD-only handlers (admin pages, config listings, kanbans,
// changeover read paths) take this as their dependency surface and
// cannot reach engine-level orchestration verbs.
//
// Phase 6.5 (2026-04-25) split this out of EngineAccess. The split
// captures the architectural role distinction surfaced in three
// independent dev reviews: most edge handlers do pure CRUD through
// services and have no business reaching engine-level orchestration.
// ServiceAccess gives those handlers a 16-method surface; orchestration
// handlers take EngineOrchestration explicitly via h.orchestration.
//
// See implementation-plan.md "Post-Phase 6 tripwires" for the
// boundary-creep guard: this split must stay at two interfaces, not
// drift into N-per-handler.
type ServiceAccess interface {
	// ── Subsystem accessors ────────────────────────────────────────
	CoreAPI() *engine.CoreClient
	AppConfig() *config.Config
	ConfigPath() string
	PLCManager() *plc.Manager
	OrderManager() *orders.Manager
	Reconciliation() *engine.ReconciliationService
	CoreSync() *engine.CoreSyncService
	// CoreNodes is a read-only snapshot of the core node map. Lives
	// on ServiceAccess (not EngineOrchestration) because it's pure
	// state read with no engine-side side effects — admin pages,
	// kanbans, and manual-order forms all need it.
	CoreNodes() map[string]protocol.NodeInfo

	// ── Service accessors ──────────────────────────────────────────
	// Phase 6.2′: per-domain services. Handlers reach single-aggregate
	// CRUD via these instead of through 50+ named *Engine methods.
	StationService() *service.StationService
	ChangeoverService() *service.ChangeoverService
	AdminService() *service.AdminService
	ProcessService() *service.ProcessService
	StyleService() *service.StyleService
	ShiftService() *service.ShiftService
	CounterService() *service.CounterService
	CatalogService() *service.CatalogService
	OrderService() *service.OrderService
}

// EngineOrchestration is the wide interface for handlers that drive
// composite-flow business operations spanning multiple subsystems
// (release flows, changeover lifecycle, lifecycle/messaging,
// WarLink tag management). Embeds ServiceAccess so orchestration
// handlers retain access to per-domain services.
//
// As services absorb orchestration logic over time, individual verbs
// migrate from this interface into ServiceAccess (via service
// accessors) and the surface here shrinks. The architectural terminus
// is EngineOrchestration becoming empty and being deleted, leaving
// ServiceAccess as the sole handler dependency.
type EngineOrchestration interface {
	ServiceAccess

	// ── Engine lifecycle / messaging ───────────────────────────────
	ApplyWarLinkConfig()
	ReconnectKafka() error
	SendEnvelope(env *protocol.Envelope) error
	RequestNodeSync()
	RequestCatalogSync()
	RequestOrderStatusSync() error
	StartupReconcile() error
	SendClaimSync()

	// ── Material orchestration ─────────────────────────────────────
	RequestNodeMaterial(nodeID int64, quantity int64) (*engine.NodeOrderResult, error)
	ReleaseNodeEmpty(nodeID int64) (*domain.Order, error)
	ReleaseNodePartial(nodeID int64, qty int64) (*domain.Order, error)
	ReleaseOrderWithLineside(orderID int64, disp engine.ReleaseDisposition) error
	ReleaseStagedOrders(nodeID int64, disp engine.ReleaseDisposition) error
	FinalizeProduceNode(nodeID int64) (*engine.NodeOrderResult, error)
	LoadBin(nodeID int64, payloadCode string, uopCount int64, manifest []protocol.IngestManifestItem) error
	ClearBin(nodeID int64) error
	RequestEmptyBin(nodeID int64, payloadCode string) (*domain.Order, error)
	RequestFullBin(nodeID int64, payloadCode string) (*domain.Order, error)

	// ── Changeover orchestration ───────────────────────────────────
	StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*domain.Changeover, error)
	CompleteProcessProductionCutover(processID int64) error
	CancelProcessChangeover(processID int64) error
	CancelProcessChangeoverRedirect(processID int64, nextStyleID *int64) error
	ReleaseChangeoverWait(processID int64, calledBy string) error
	StageNodeChangeoverMaterial(processID, nodeID int64) (*domain.Order, error)
	EmptyNodeForToolChange(processID, nodeID int64, partialQty int64) (*domain.Order, error)
	ReleaseNodeIntoProduction(processID, nodeID int64) (*domain.Order, error)
	SwitchNodeToTarget(processID, nodeID int64) error
	SwitchOperatorStationToTarget(processID, stationID int64) error
	SyncProcessCounter(processID int64) error
	FlipABNode(nodeID int64) error

	// ── WarLink tag management ─────────────────────────────────────
	EnsureTagPublished(rpID int64, plcName, tagName string)
	ManageReportingPointTag(rpID int64, oldPLC, oldTag string, oldManaged bool, newPLC, newTag string)
	CleanupReportingPointTag(rpID int64, plcName, tagName string, managed bool)
}

// Compile-time assertions: *engine.Engine must satisfy both interfaces.
// EngineOrchestration embeds ServiceAccess, so the second assertion
// implies the first; both kept here for explicit boundary documentation.
var (
	_ ServiceAccess       = (*engine.Engine)(nil)
	_ EngineOrchestration = (*engine.Engine)(nil)
)
