package www

import (
	"context"
	"time"

	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/dispatch/eta"
	"shingocore/domain"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/messaging"
	"shingocore/service"
)

// ServiceAccess is the narrow interface that service-shaped www handlers
// require from the engine: subsystem accessors + per-domain service
// accessors + read-only state queries. CRUD-only handlers (admin pages,
// listings, robot status views) take this as their dependency surface
// and cannot reach engine-level orchestration verbs.
//
// Phase 6.5 (2026-04-25) split this out of EngineAccess. The split
// captures the architectural role distinction: most handlers do pure
// CRUD through services and have no business reaching engine-level
// orchestration. ServiceAccess gives those handlers a 25-method surface;
// orchestration handlers take EngineOrchestration explicitly via
// h.orchestration.
//
// State queries (GetCachedRobotStatus, GetAllCachedRobots,
// GetNodeOccupancy) live here despite being engine-side because they
// are pure reads with no side effects — semantically equivalent to
// service queries from the handler's perspective.
//
// See implementation-plan.md "Post-Phase 6 tripwires" for the
// boundary-creep guard: this split must stay at two interfaces, not
// drift into N-per-handler.
type ServiceAccess interface {
	// ── Subsystem accessors ────────────────────────────────────────
	AppConfig() *config.Config
	ConfigPath() string
	Dispatcher() *dispatch.Dispatcher
	Fleet() fleet.Backend
	Tracker() fleet.OrderTracker
	MsgClient() *messaging.Client
	RequestEdgeReregister(station string) error // Q-034: ask edge(s) to re-send their catalog
	EventBus() *engine.EventBus
	Reconciliation() *engine.ReconciliationService
	Recovery() *engine.RecoveryService

	// ── Service accessors ──────────────────────────────────────────
	// Phase 3a: per-domain services. Handlers reach single-aggregate
	// CRUD via these instead of through named *Engine methods.
	BinManifest() *service.BinManifestService
	BinService() *service.BinService
	OrderService() *service.OrderService
	NodeService() *service.NodeService
	AuditService() *service.AuditService
	DemandService() *service.DemandService
	// DemandEpisodeService reads the demand grain (Phase 6). Distinct from
	// DemandService, which is the production-quota CRUD behind /demand.
	DemandEpisodeService() *service.DemandEpisodeService
	LoaderService() *service.LoaderService
	CalculatorService() *service.ThresholdCalculatorService
	PayloadService() *service.PayloadService
	MissionService() *service.MissionService
	TestCommandService() *service.TestCommandService
	CMSTransactionService() *service.CMSTransactionService
	InventoryService() *service.InventoryService
	InventoryDeltaService() *service.InventoryDeltaService
	AdminService() *service.AdminService
	HealthService() *service.HealthService
	DashboardService() *service.DashboardService
	FootprintService() *service.FootprintService
	PartsService() *service.PartsService
	HeartbeatService() *service.HeartbeatService

	// ── Read-only state queries ────────────────────────────────────
	// These look like orchestration verbs but are pure reads with no
	// engine-side side effects. Robot status views and node-occupancy
	// listings need them; those are CRUD-shaped handlers, not
	// orchestration handlers.
	GetCachedRobotStatus(vehicleID string) (fleet.RobotStatus, bool)
	GetAllCachedRobots() []fleet.RobotStatus
	GetNodeOccupancy() ([]engine.OccupancyEntry, error)
	RobotGroups() ([]fleet.RobotGroup, error)
	// SourceabilityPage returns the read model for the Core sourcing page — the
	// gated per-(process, style) verdicts plus claim/pool drill-in context and
	// the replenishment queue. A pure read of the monitor snapshot.
	SourceabilityPage() (engine.SourceabilityPageView, error)
	// ReplenishmentHealth is the per-payload inventory rollup behind the
	// inventory page: DB on-hand (bins + lineside split), the threshold monitor's
	// cached total (for drift detection), and configured thresholds. A pure read
	// of the monitor snapshot + inventory.
	ReplenishmentHealth(ctx context.Context) ([]engine.PayloadHealth, error)
	// MaintainedGroupStates is the keeper's last tick, one line per (group, bin
	// type): the declared level and the three populations it subtracted to reach
	// its gap.
	//
	// A SNAPSHOT OF A COMPLETED TICK, not a fresh computation. The page shows what
	// the keeper actually decided, so an operator reading a surprising gap is
	// reading the arithmetic that produced the asks in front of them rather than a
	// second opinion computed at render time. Empty until the first tick, and
	// empty forever on a plant with no maintained group.
	MaintainedGroupStates() []engine.MaintainerGroupState

	// Ledger-integrity exception list (Phase 4.6). Read-side only.
	OpenNegativeBins() ([]domain.OpenNegativeBin, error)
	NegativeLedgerPayloads() (map[string]int, error)
	NegativeLedgerExcursions(since time.Time, releaseWindow time.Duration, limit int) ([]domain.NegativeExcursion, error)
	// DeltaIntegrityByPayload is the mechanism panel beside the exception
	// list: how much count was dropped per payload, against that payload's
	// current ledger total. Also read-side only.
	DeltaIntegrityByPayload(since time.Time) ([]domain.DeltaIntegrity, error)
	// DeltaIntegrityDaily is the same population on the time axis, bucketed by
	// plant-local day. Rides the same request as the panel above.
	DeltaIntegrityDaily(since time.Time, tz string) ([]domain.DeltaDay, error)
	// CarrierBindings is 5.11's ledger half: every carrier, the payload ShinGo
	// believes it holds, and when that belief last started. Unfiltered — the
	// candidate rule is a pure function in this package.
	CarrierBindings() ([]domain.CarrierBinding, error)

	// SourceabilityEvents is the persisted verdict-change history (Phase 5).
	SourceabilityEvents(since time.Time, processID, payload string, limit int) ([]domain.SourceabilityEvent, error)
	// ValidateAdvancedLoadSequence checks a payload's configured load-sequence
	// task names against the RDS binTask keys of its assigned node locations. A
	// pure read (no side effects): a missing key at a real location returns an
	// error (reject the save); an un-checkable case (no RDS, no nodes) returns a
	// check with warnings and err=nil (save allowed, flagged unverified).
	ValidateAdvancedLoadSequence(payloadID int64, seqName string) (*engine.LoadSequenceCheck, error)
	EtaCache() *eta.Cache
	GetActiveOrdersWithRobotLocation() ([]engine.BoardOrder, error)
	GetActiveOrdersWithRobotLocationFiltered(stations []string) ([]engine.BoardOrder, error)
	GetActiveOrderWithRobotLocation(orderID int64) (*engine.BoardOrder, error)
}

// EngineOrchestration is the wide interface for handlers that drive
// composite-flow business operations spanning multiple subsystems
// (corrections, direct orders, scene sync, cross-edge messaging,
// live reconfiguration). Embeds ServiceAccess so orchestration
// handlers retain access to per-domain services.
//
// As services absorb orchestration logic over time, individual verbs
// migrate from this interface into ServiceAccess (via service
// accessors) and the surface here shrinks. The architectural terminus
// is EngineOrchestration becoming empty and being deleted, leaving
// ServiceAccess as the sole handler dependency.
type EngineOrchestration interface {
	ServiceAccess

	// ── Corrections ────────────────────────────────────────────────
	ApplyCorrection(req engine.ApplyCorrectionRequest) (int64, error)
	ApplyBatchCorrection(req engine.BatchCorrectionRequest) error

	// ── Orders ─────────────────────────────────────────────────────
	CreateBinMove(req engine.BinMoveRequest) (*engine.BinMoveResult, error)
	TerminateOrder(orderID int64, actor string) error
	// HardReleaseOrder advances a dwelling order past its wait regardless of who
	// owns it — the escape hatch for a wait whose ordinary releaser is wedged.
	// Same privilege class as TerminateOrder; the audit row names the actor.
	HardReleaseOrder(orderID int64, actor string) error

	// ── Scene sync ─────────────────────────────────────────────────
	SceneSync() (int, int, int, error)
	SyncScenePoints(areas []fleet.SceneArea) (int, map[string]string)
	UpdateNodeZones(locMap map[string]string, overwrite bool)

	// ── Messaging ──────────────────────────────────────────────────
	SendDataToEdge(subject string, stationID string, payload any) error

	// ── Live reconfiguration ───────────────────────────────────────
	ReconfigureDatabase()
	ReconfigureFleet()
	ReconfigureMessaging()
	ReconfigureCountGroups()
	ReconfigureNotifications()
}

// Compile-time assertions: *engine.Engine must satisfy both interfaces.
// EngineOrchestration embeds ServiceAccess, so the second assertion
// implies the first; both kept here for explicit boundary documentation.
var (
	_ ServiceAccess       = (*engine.Engine)(nil)
	_ EngineOrchestration = (*engine.Engine)(nil)
)
