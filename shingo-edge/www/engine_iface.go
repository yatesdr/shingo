package www

import (
	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/engine/changeover"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/service"
)

// ServiceAccess is the narrow interface that service-shaped www handlers
// require from the engine: subsystem accessors + per-domain service
// accessors. CRUD-only handlers (admin pages, config listings, orders,
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
	// orders, and manual-order forms all need it.
	CoreNodes() map[string]protocol.NodeInfo
	// PayloadBinTypes returns the cached payload→dunnage mapping delivered
	// by Core on each NodeListResponse. Used by the operator-station view
	// handler to populate the dunnage picker without a per-node query.
	PayloadBinTypes() []protocol.PayloadBinTypeInfo

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
	// SourcingStateForProcess returns the cached sourceability verdicts for a
	// process (by Name) so the changeover picker can annotate each style.
	SourcingStateForProcess(process string) []protocol.SourcingState
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

	// ── Material orchestration ─────────────────────────────────────
	RequestNodeMaterial(nodeID int64, quantity int64) (*engine.NodeOrderResult, error)
	ReleaseNodeEmpty(nodeID int64) (*domain.Order, error)
	ReleaseNodePartial(nodeID int64, qty int64) (*domain.Order, error)
	ReleaseNodeWithRemainingUOP(nodeID int64, qty int64, remainingUOP int) (*domain.Order, error)
	ReleaseOrderWithLineside(orderID int64, disp engine.ReleaseDisposition) error
	ReleaseStagedOrders(nodeID int64, disp engine.ReleaseDisposition) error
	RequestProduceSwap(nodeID int64) (*engine.NodeOrderResult, error)
	LoadBin(nodeID int64, payloadCode string, uopCount int64, manifest []protocol.IngestManifestItem) error
	ClearBin(nodeID int64, binTypeCode string) error
	ClearLoaderHome(nodeID int64) error
	EnrichHomeBufferPartials(nodes []domain.StationNodeView)
	FetchMarketBins(nodeID int64) ([]engine.MarketBinInfo, error)
	PullFromMarket(nodeID int64, sourceCoreName string) error
	PushEmptyOut(nodeID int64) error
	RequestEmptyBin(nodeID int64, payloadCode string) (*domain.Order, error)
	RequestFullBin(nodeID int64, payloadCode string) (*domain.Order, error)

	// ── Changeover orchestration ───────────────────────────────────
	PreviewChangeoverPlan(processID, toStyleID int64) (changeover.Plan, error)
	StartProcessChangeover(processID, toStyleID int64, calledBy, notes string) (*domain.Changeover, error)
	CompleteProcessProductionCutover(processID int64) error
	CancelProcessChangeover(processID int64) error
	CancelProcessChangeoverRedirect(processID int64, nextStyleID *int64) error
	ReleaseChangeoverWait(processID int64, disp engine.ReleaseDisposition) (engine.ReleaseChangeoverWaitResult, error)
	// ReleaseChangeoverWaitForNode scopes the same release to one node's task.
	ReleaseChangeoverWaitForNode(processID, processNodeID int64, disp engine.ReleaseDisposition) (engine.ReleaseChangeoverWaitResult, error)
	// AbandonChangeoverSupply is the operator exit from awaiting_material:
	// cancel the parked supply half (both halves unless acceptHalf) and land
	// the node task abandoned-terminal.
	AbandonChangeoverSupply(processID, processNodeID int64, acceptHalf bool, calledBy string) error
	// ChangeoverGateStatus is a pure read of the cutover gate — safe to poll.
	ChangeoverGateStatus(processID int64) (bool, []domain.Blocker, error)
	SequentialChangeoverCutover(processID, nodeID int64, calledBy string) error
	StageNodeChangeoverMaterial(processID, nodeID int64) (*domain.Order, error)
	EvacuateNode(processID, nodeID int64, partialQty int64) (*domain.Order, error)
	DeliverNewMaterialForChangeover(processID, nodeID int64) (*domain.Order, error)
	SwitchNodeToTarget(processID, nodeID int64) error
	SwitchOperatorStationToTarget(processID, stationID int64) error
	SyncProcessCounter(processID int64) error
	FlipABNode(nodeID int64) error

	// ── UOP backfill (admin) ───────────────────────────────────────
	// Item 3: seeds Core's lineside_buckets from Edge state. Auto-fires
	// at startup via main.go; the admin endpoint exists for re-runs.
	BackfillBucketsForStation(force bool) (int, error)
	BucketBackfillNeeded() (bool, error)

	// ── Lineside admin (team leader / engineer override) ───────────
	// Backs the "Lineside Buckets" admin page. clearBucket=true sets
	// the bucket qty to 0 (deleting the row); clearBucket=false sets
	// qty to targetQty exactly. Either way emits a LinesideBucketDelta
	// with ReasonOperatorCorrectionBucket so Core mirrors.
	AdminAdjustLinesideBucket(bucketID int64, targetQty int, clearBucket bool) error

	// ── UOP-threshold replenishment admin ──────────────────────────
	// Backs the CELL half of the /replenishment admin page. The loader
	// half was deleted: Core owns the loader UOP threshold and the Edge
	// write path ended in a no-op stub. See engine/replenishment_admin.go.
	UpdateCellReorder(engine.CellReorderInput) error

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
