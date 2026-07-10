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

	"shingo/protocol/types"
	"shingocore/config"
	"shingocore/countgroup"
	"shingocore/dispatch"
	"shingocore/dispatch/eta"
	"shingocore/fleet"
	"shingocore/fulfillment"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/orders"
)

type LogFunc = types.DebugLogFunc

type Config struct {
	AppConfig  *config.Config
	ConfigPath string
	DB         *store.DB
	Fleet      fleet.Backend
	MsgClient  *messaging.Client
	LogFunc    LogFunc
	DebugLog   types.DebugLogFunc
}

type Engine struct {
	cfg                   *config.Config
	configPath            string
	db                    *store.DB
	fleet                 fleet.Backend
	msgClient             *messaging.Client
	dispatcher            *dispatch.Dispatcher
	tracker               fleet.OrderTracker
	countGroup            *countgroup.Runner                          // nil if feature disabled / no groups configured
	countGroupMu          sync.Mutex                                  // guards the countGroup pointer (ReconfigureCountGroups vs Start/Stop)
	countGroupBuild       func(countgroup.Emitter) *countgroup.Runner // stored for ReconfigureCountGroups
	Events                *EventBus
	logFn                 LogFunc
	debugLog              types.DebugLogFunc
	reconciliation        *ReconciliationService
	recovery              *RecoveryService
	fulfillment           *fulfillment.Scanner
	binManifest           *service.BinManifestService
	binService            *service.BinService
	orderService          *service.OrderService
	nodeService           *service.NodeService
	auditService          *service.AuditService
	demandService         *service.DemandService
	loaderService         *service.LoaderService
	calculatorService     *service.ThresholdCalculatorService
	payloadService        *service.PayloadService
	missionService        *service.MissionService
	testCmdService        *service.TestCommandService
	cmsTxnService         *service.CMSTransactionService
	inventoryService      *service.InventoryService
	adminService          *service.AdminService
	healthService         *service.HealthService
	tagVerifyService      *service.TagVerifyService
	inventoryDeltaService *service.InventoryDeltaService
	dashboardService      *service.DashboardService
	footprintService      *service.FootprintService
	partsService          *service.PartsService
	heartbeatService      *service.HeartbeatService
	thresholdMonitor      *ThresholdMonitor
	etaCache              *eta.Cache
	stopChan              chan struct{}
	stopOnce              sync.Once
	sceneSyncing          atomic.Bool
	fleetConnected        atomic.Bool
	msgConnected          atomic.Bool
	dbConnected           atomic.Bool
	robotsMu              sync.RWMutex
	robotsCache           map[string]fleet.RobotStatus

	// preDisconnectAvailability captures per-robot Available state at the
	// moment a fleet disconnect is detected. autoResumeAfterFleetReconnect
	// consumes it on the next reconnect to resume only robots we last saw
	// running (operator-paused robots stay paused). Single-shot: cleared
	// when consumed; nil between cycles.
	//
	// Single-writer: written and read only by checkConnectionStatus, which
	// runs from connectionHealthLoop's single goroutine. The auto-resume
	// goroutine receives a copy as a function argument and never touches
	// this field directly — so no lock is needed here. robotsCache reads
	// during the capture still take robotsMu in the usual way.
	preDisconnectAvailability map[string]bool
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
	// confirmDelivered late-binds to e.dispatcher. Engine.New leaves
	// dispatcher nil; Start() constructs it (engine_lifecycle.go) and the
	// reconciliation Loop also starts from Start(), after dispatcher is
	// non-nil. Routing through ConfirmReceipt drives the (Delivered →
	// Confirmed) actionMap, which fires fireCompleted → EmitOrderCompleted
	// so Edge actually sees the transition (pre-fix the direct DB write
	// stranded Edge at delivered indefinitely).
	e.reconciliation.confirmDelivered = func(order *orders.Order) error {
		_, err := e.dispatcher.Lifecycle().ConfirmReceipt(order, order.StationID, "auto_confirm_timeout", 0)
		return err
	}
	// abandonOrder cancels a stuck order via the standard teardown and
	// cascades to its two-robot sibling so a swap tears down as a unit
	// (CancelOrder is idempotent if the sibling is already terminal).
	e.reconciliation.abandonOrder = func(order *orders.Order, reason string) error {
		lc := e.dispatcher.Lifecycle()
		lc.CancelOrder(order, order.StationID, reason)
		if sibUUID, serr := e.db.OrderSiblingUUID(order.ID); serr == nil && sibUUID != "" {
			if sib, gerr := e.db.GetOrderByUUID(sibUUID); gerr == nil && sib != nil {
				lc.CancelOrder(sib, sib.StationID, reason)
			}
		}
		return nil
	}
	// advanceCompound re-drives a compound (reshuffle) parent stranded in
	// `reshuffling` with all children terminal — the liveness backstop. Late-bound
	// (e.dispatcher is created in Start()); the closure reads it at call time.
	e.reconciliation.advanceCompound = func(parentID int64) error {
		return e.dispatcher.AdvanceCompoundOrder(parentID)
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
	e.tagVerifyService = service.NewTagVerifyService(e.db)
	e.inventoryDeltaService = service.NewInventoryDeltaService(e.db, e.binManifest)
	e.dashboardService = service.NewDashboardService(e.db)
	e.footprintService = service.NewFootprintService(e.db)
	e.partsService = service.NewPartsService(e.db)
	e.heartbeatService = service.NewHeartbeatService(e.db)
	e.thresholdMonitor = NewThresholdMonitor(e)
	// Loader CRUD re-derives demand_registry + nudges the monitor on each edit.
	e.loaderService = service.NewLoaderService(e.db, e.thresholdMonitor)
	e.calculatorService = service.NewThresholdCalculatorService(e.db)
	e.etaCache = eta.NewCache(e.db.DB)
	return e
}

// ThresholdMonitor returns the UOP-threshold monitor for wiring into
// the messaging layer's CoreDataService. Public so cmd/shingocore can
// thread the dependency without engine internals leaking elsewhere.
func (e *Engine) ThresholdMonitor() *ThresholdMonitor {
	return e.thresholdMonitor
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
