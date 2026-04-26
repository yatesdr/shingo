package www

import (
	"shingocore/config"
	"shingocore/dispatch"
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
	PayloadService() *service.PayloadService
	MissionService() *service.MissionService
	TestCommandService() *service.TestCommandService
	CMSTransactionService() *service.CMSTransactionService
	InventoryService() *service.InventoryService
	AdminService() *service.AdminService
	HealthService() *service.HealthService

	// ── Read-only state queries ────────────────────────────────────
	// These look like orchestration verbs but are pure reads with no
	// engine-side side effects. Robot status views and node-occupancy
	// listings need them; those are CRUD-shaped handlers, not
	// orchestration handlers.
	GetCachedRobotStatus(vehicleID string) (fleet.RobotStatus, bool)
	GetAllCachedRobots() []fleet.RobotStatus
	GetNodeOccupancy() ([]engine.OccupancyEntry, error)
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
	CreateDirectOrder(req engine.DirectOrderRequest) (*engine.DirectOrderResult, error)
	TerminateOrder(orderID int64, actor string) error

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
}

// Compile-time assertions: *engine.Engine must satisfy both interfaces.
// EngineOrchestration embeds ServiceAccess, so the second assertion
// implies the first; both kept here for explicit boundary documentation.
var (
	_ ServiceAccess       = (*engine.Engine)(nil)
	_ EngineOrchestration = (*engine.Engine)(nil)
)
