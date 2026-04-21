// engine.go — ShinGo Core engine struct + construction.
//
// Stage 7 split the former 682-LOC file by concern; only the struct,
// New(), dbg(), and the robot status cache getters remain here.
// Sibling files:
//
//   engine_lifecycle.go   Start, Stop, loadActiveOrders
//   engine_accessors.go   one-liner subsystem getters + SetCountGroupRunner
//   engine_messaging.go   SendToEdge, SendDataToEdge, RunFulfillmentScan
//   engine_connection.go  checkConnectionStatus, connectionHealthLoop
//   engine_reconfigure.go ReconfigureDatabase/Fleet/CountGroups/Messaging
//   engine_scene_sync.go  SyncScenePoints, SyncFleetNodes, UpdateNodeZones, SceneSync
//   engine_background.go  robotRefreshLoop, stagedBinSweepLoop
//
// The fulfillment scanner lives in the shingocore/fulfillment
// sub-package (Stage 7 extraction); the engine holds a pointer to
// *fulfillment.Scanner and the call sites in wiring.go use its
// Trigger / RunOnce methods directly.

package engine

import (
	"log"
	"sync"
	"sync/atomic"

	"shingocore/config"
	"shingocore/countgroup"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fulfillment"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
)

type LogFunc func(format string, args ...any)

type Config struct {
	AppConfig  *config.Config
	ConfigPath string
	DB         *store.DB
	Fleet      fleet.Backend
	MsgClient  *messaging.Client
	LogFunc    LogFunc
	DebugLog   func(string, ...any)
}

type Engine struct {
	cfg             *config.Config
	configPath      string
	db              *store.DB
	fleet           fleet.Backend
	msgClient       *messaging.Client
	dispatcher      *dispatch.Dispatcher
	tracker         fleet.OrderTracker
	countGroup      *countgroup.Runner                          // nil if feature disabled / no groups configured
	countGroupBuild func(countgroup.Emitter) *countgroup.Runner // stored for ReconfigureCountGroups
	Events          *EventBus
	logFn           LogFunc
	debugLog        func(string, ...any)
	reconciliation  *ReconciliationService
	recovery        *RecoveryService
	fulfillment     *fulfillment.Scanner
	binManifest     *service.BinManifestService
	binService      *service.BinService
	orderService    *service.OrderService
	nodeService     *service.NodeService
	auditService    *service.AuditService
	demandService   *service.DemandService
	payloadService  *service.PayloadService
	missionService  *service.MissionService
	testCmdService  *service.TestCommandService
	cmsTxnService   *service.CMSTransactionService
	inventoryService *service.InventoryService
	adminService    *service.AdminService
	healthService   *service.HealthService
	stopChan        chan struct{}
	stopOnce        sync.Once
	sceneSyncing    atomic.Bool
	fleetConnected  atomic.Bool
	msgConnected    atomic.Bool
	dbConnected     atomic.Bool
	robotsMu        sync.RWMutex
	robotsCache     map[string]fleet.RobotStatus
}

func New(c Config) *Engine {
	logFn := c.LogFunc
	if logFn == nil {
		logFn = log.Printf
	}
	e := &Engine{
		cfg:         c.AppConfig,
		configPath:  c.ConfigPath,
		db:          c.DB,
		fleet:       c.Fleet,
		msgClient:   c.MsgClient,
		Events:      NewEventBus(),
		logFn:       logFn,
		debugLog:    c.DebugLog,
		stopChan:    make(chan struct{}),
		robotsCache: make(map[string]fleet.RobotStatus),
	}
	e.reconciliation = newReconciliationService(e.db, e.logFn)
	e.reconciliation.onOrderCompleted = func(orderID int64, edgeUUID, stationID string) {
		e.handleOrderCompleted(OrderCompletedEvent{
			OrderID:   orderID,
			EdgeUUID:  edgeUUID,
			StationID: stationID,
		})
	}
	e.recovery = newRecoveryService(e)
	e.binManifest = service.NewBinManifestService(e.db)
	e.binService = service.NewBinService(e.db, e.binManifest)
	e.orderService = service.NewOrderService(e.db, e.fleet)
	e.nodeService = service.NewNodeService(e.db)
	e.auditService = service.NewAuditService(e.db)
	e.demandService = service.NewDemandService(e.db)
	e.payloadService = service.NewPayloadService(e.db)
	e.missionService = service.NewMissionService(e.db)
	e.testCmdService = service.NewTestCommandService(e.db)
	e.cmsTxnService = service.NewCMSTransactionService(e.db)
	e.inventoryService = service.NewInventoryService(e.db)
	e.adminService = service.NewAdminService(e.db)
	e.healthService = service.NewHealthService(e.db)
	return e
}

func (e *Engine) dbg(format string, args ...any) {
	if fn := e.debugLog; fn != nil {
		fn(format, args...)
	}
}

// GetCachedRobotStatus returns the last known status of a robot from the in-memory cache.
// The cache is refreshed every 2 seconds by robotRefreshLoop.
func (e *Engine) GetCachedRobotStatus(vehicleID string) (fleet.RobotStatus, bool) {
	e.robotsMu.RLock()
	defer e.robotsMu.RUnlock()
	r, ok := e.robotsCache[vehicleID]
	return r, ok
}

// GetAllCachedRobots returns a snapshot of all cached robot statuses.
func (e *Engine) GetAllCachedRobots() []fleet.RobotStatus {
	e.robotsMu.RLock()
	defer e.robotsMu.RUnlock()
	robots := make([]fleet.RobotStatus, 0, len(e.robotsCache))
	for _, r := range e.robotsCache {
		robots = append(robots, r)
	}
	return robots
}
