// engine.go — ShinGo Edge engine: struct, lifecycle, and subsystem wiring.
//
// Layout:
//   Types / Engine struct       – dependencies and internal state
//   New / Start / Stop          – lifecycle (Start creates managers, wires
//                                 event handlers, restores changeover state,
//                                 starts PLC polling)
//   Accessors                   – one-liner getters for subsystems
//   Core node sync              – SetCoreNodes, CoreNodes
//   Func injection              – SetNodeSyncFunc, SetCatalogSyncFunc,
//                                 SetSendFunc, SetKafkaReconnectFunc
//   Payload catalog             – HandlePayloadCatalog
//   Outbound messaging          – SendEnvelope, ReconnectKafka
//   WarLink                     – ApplyWarLinkConfig

package engine

import (
	"fmt"
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/protocol/types"
	"shingoedge/config"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/catalog"
)

// ── Types & struct ──────────────────────────────────────────────────

// LogFunc is the logging callback signature.
type LogFunc func(format string, args ...any)

// DebugLogFunc is a nil-safe debug logging function.
type DebugLogFunc = types.DebugLogFunc

// Engine centralizes all business logic and orchestrates subsystems.
type Engine struct {
	cfg         *config.Config
	configPath  string
	db          *store.DB
	logFn       LogFunc
	debugFn     DebugLogFunc
	debugLogger *debuglog.Logger

	plcMgr   *plc.Manager
	orderMgr *orders.Manager

	hourlyTracker  *HourlyTracker
	reconciliation *ReconciliationService
	coreSync       *CoreSyncService

	// Service layer (canonical caller surface for handlers + engine
	// business logic). Phase 6.1 introduced the cross-aggregate
	// coordinators (stationService, changeoverService); Phase 6.2′
	// completed the per-domain extraction, deleting
	// engine_db_methods.go and dropping EngineAccess to ~30 methods;
	// Phase 6.5 split that into ServiceAccess (16 methods) +
	// EngineOrchestration (35 verbs, embeds ServiceAccess).
	stationService    *service.StationService
	changeoverService *service.ChangeoverService
	adminService      *service.AdminService
	processService    *service.ProcessService
	styleService      *service.StyleService
	shiftService      *service.ShiftService
	counterService    *service.CounterService
	catalogService    *service.CatalogService
	orderService      *service.OrderService

	coreClient    *CoreClient
	coreNodes     map[string]protocol.NodeInfo
	coreNodesMu   sync.RWMutex
	nodeSyncFn    func()
	catalogSyncFn func()
	sendFn        func(*protocol.Envelope) error
	kafkaReconnFn func() error

	Events    *EventBus
	stopChan  chan struct{}
	startedAt time.Time
}

// Config holds the parameters needed to create an Engine.
type Config struct {
	AppConfig   *config.Config
	ConfigPath  string
	DB          *store.DB
	LogFunc     LogFunc
	DebugLogger *debuglog.Logger
}

// ── Lifecycle ───────────────────────────────────────────────────────

// New creates a new Engine. Call Start() to initialize and wire subsystems.
func New(c Config) *Engine {
	logFn := c.LogFunc
	if logFn == nil {
		logFn = func(string, ...interface{}) {}
	}
	var debugFn DebugLogFunc
	if c.DebugLogger != nil {
		debugFn = DebugLogFunc(c.DebugLogger.Func("engine"))
	}
	e := &Engine{
		cfg:         c.AppConfig,
		configPath:  c.ConfigPath,
		db:          c.DB,
		logFn:       logFn,
		debugFn:     debugFn,
		debugLogger: c.DebugLogger,
		Events:      NewEventBus(),
		stopChan:    make(chan struct{}),
	}
	e.coreClient = NewCoreClient(c.AppConfig.CoreAPI)
	e.reconciliation = newReconciliationService(e.db)
	e.coreSync = newCoreSyncService(e)
	e.stationService = service.NewStationService(e.db)
	e.changeoverService = service.NewChangeoverService(e.db)
	e.adminService = service.NewAdminService(e.db)
	e.processService = service.NewProcessService(e.db)
	e.styleService = service.NewStyleService(e.db)
	e.shiftService = service.NewShiftService(e.db)
	e.counterService = service.NewCounterService(e.db)
	e.catalogService = service.NewCatalogService(e.db)
	e.orderService = service.NewOrderService(e.db)
	return e
}

// Start creates all managers, wires event handlers, and starts subsystems.
func (e *Engine) Start() {
	// Create subsystem emitter adapters
	plcEmit := &plcEmitter{bus: e.Events}
	orderEmit := &orderEmitter{bus: e.Events}

	// Create managers
	e.plcMgr = plc.NewManager(e.db, e.cfg, plcEmit)
	e.orderMgr = orders.NewManager(e.db, orderEmit, e.cfg.StationID())

	// Wire debug logging to subsystems
	if e.debugLogger != nil {
		e.plcMgr.DebugLog = plc.DebugLogFunc(e.debugLogger.Func("plc"))
		e.orderMgr.DebugLog = orders.DebugLogFunc(e.debugLogger.Func("orders"))
	}
	e.hourlyTracker = NewHourlyTracker(e.db, e.cfg.Timezone)

	// Wire the event chain
	e.wireEventHandlers()

	// Reconcile any in-progress changeover against order statuses
	e.restoreChangeoverState()

	// Note: pre-side-cycle this had a NOTE about StartupSweepManualSwap.
	// That sweep, along with HandleDemandSignal and tryAutoRequest, was
	// removed once the side-cycle (line REQUEST -> loader L1 -> L2) became
	// the canonical empty-in path. Loaders no longer need a startup-time
	// kick to begin pulling empties.

	// Start WarLink poller and counter polling
	if e.cfg.WarLink.Enabled {
		e.plcMgr.StartWarLinkPoller()
	}
	e.plcMgr.StartPolling()

	e.startedAt = time.Now()
	e.logFn("Engine started: namespace=%s line_id=%s", e.cfg.Namespace, e.cfg.LineID)
}

// ── Accessors ───────────────────────────────────────────────────────

// Uptime returns the number of seconds since the engine started.
func (e *Engine) Uptime() int64 {
	return int64(time.Since(e.startedAt).Seconds())
}

// Stop shuts down all subsystems gracefully.
func (e *Engine) Stop() {
	select {
	case <-e.stopChan:
	default:
		close(e.stopChan)
	}
	if e.plcMgr != nil {
		e.plcMgr.Stop()
	}
	e.logFn("Engine stopped")
}

// ── WarLink (PLC connectivity) ──────────────────────────────────────

// ApplyWarLinkConfig stops and restarts the WarLink poller/SSE to match the current config.
// Always stops first to handle mode switches (poll→sse or sse→poll) cleanly.
// Rebuilds the WarLink HTTP client in case host/port changed.
func (e *Engine) ApplyWarLinkConfig() {
	e.plcMgr.StopWarLinkPoller()
	e.plcMgr.ReplaceClient(plc.NewWarlinkClient(
		fmt.Sprintf("http://%s:%d/api", e.cfg.WarLink.Host, e.cfg.WarLink.Port),
	))
	if e.cfg.WarLink.Enabled {
		e.plcMgr.StartWarLinkPoller()
	}
}

// DB returns the database handle.
func (e *Engine) DB() *store.DB { return e.db }

// CoreAPI returns the Core HTTP client for telemetry requests.
func (e *Engine) CoreAPI() *CoreClient { return e.coreClient }

// Config returns the app config.
func (e *Engine) AppConfig() *config.Config { return e.cfg }

// ConfigPath returns the config file path.
func (e *Engine) ConfigPath() string { return e.configPath }

// PLCManager returns the PLC manager.
func (e *Engine) PLCManager() *plc.Manager { return e.plcMgr }

// OrderManager returns the order manager.
func (e *Engine) OrderManager() *orders.Manager          { return e.orderMgr }
func (e *Engine) Reconciliation() *ReconciliationService { return e.reconciliation }
func (e *Engine) CoreSync() *CoreSyncService             { return e.coreSync }

// Service-layer accessors. Phase 6.1 introduced the cross-aggregate
// coordinators (StationService, ChangeoverService); Phase 6.2′ added
// the remaining per-domain services and deleted engine_db_methods.go.
// All handler call sites reach the persistence layer through these
// services; engine internals can call them or the underlying
// *store.DB depending on whether they need orchestration semantics.
func (e *Engine) StationService() *service.StationService       { return e.stationService }
func (e *Engine) ChangeoverService() *service.ChangeoverService { return e.changeoverService }
func (e *Engine) AdminService() *service.AdminService           { return e.adminService }
func (e *Engine) ProcessService() *service.ProcessService       { return e.processService }
func (e *Engine) StyleService() *service.StyleService           { return e.styleService }
func (e *Engine) ShiftService() *service.ShiftService           { return e.shiftService }
func (e *Engine) CounterService() *service.CounterService       { return e.counterService }
func (e *Engine) CatalogService() *service.CatalogService       { return e.catalogService }
func (e *Engine) OrderService() *service.OrderService           { return e.orderService }

// ── Core node sync ──────────────────────────────────────────────────

// SetCoreNodes updates the core node set and emits EventCoreNodesUpdated.
func (e *Engine) SetCoreNodes(nodes []protocol.NodeInfo) {
	e.coreNodesMu.Lock()
	e.coreNodes = make(map[string]protocol.NodeInfo, len(nodes))
	for _, n := range nodes {
		e.coreNodes[n.Name] = n
	}
	e.coreNodesMu.Unlock()

	e.Events.Emit(Event{
		Type:      EventCoreNodesUpdated,
		Timestamp: time.Now(),
		Payload:   CoreNodesUpdatedEvent{Nodes: nodes},
	})
}

// CoreNodes returns a copy of the core node set.
func (e *Engine) CoreNodes() map[string]protocol.NodeInfo {
	e.coreNodesMu.RLock()
	defer e.coreNodesMu.RUnlock()
	cp := make(map[string]protocol.NodeInfo, len(e.coreNodes))
	for k, v := range e.coreNodes {
		cp[k] = v
	}
	return cp
}

// ── Func injection (wired by main.go) ───────────────────────────────

// SetNodeSyncFunc sets the function to call when a node sync is requested.
func (e *Engine) SetNodeSyncFunc(fn func()) {
	e.nodeSyncFn = fn
}

// RequestNodeSync triggers a node list request to core.
func (e *Engine) RequestNodeSync() {
	if e.nodeSyncFn != nil {
		e.nodeSyncFn()
	}
}

// SetCatalogSyncFunc sets the function to call when a payload catalog sync is requested.
func (e *Engine) SetCatalogSyncFunc(fn func()) {
	e.catalogSyncFn = fn
}

// RequestCatalogSync triggers a payload catalog request to core.
func (e *Engine) RequestCatalogSync() {
	if e.catalogSyncFn != nil {
		e.catalogSyncFn()
	}
}

// ── Payload catalog ─────────────────────────────────────────────────

// HandlePayloadCatalog upserts payload catalog entries received from core and
// prunes any local entries that no longer exist in core's response.
func (e *Engine) HandlePayloadCatalog(entries []protocol.CatalogPayloadInfo) {
	ids := make([]int64, 0, len(entries))
	for _, b := range entries {
		entry := &catalog.CatalogEntry{
			ID: b.ID, Name: b.Name, Code: b.Code,
			Description: b.Description,
			UOPCapacity: b.UOPCapacity,
		}
		if err := e.db.UpsertPayloadCatalog(entry); err != nil {
			log.Printf("engine: upsert payload catalog entry %s: %v", b.Name, err)
		}
		ids = append(ids, b.ID)
	}
	if err := e.db.DeleteStalePayloadCatalogEntries(ids); err != nil {
		log.Printf("engine: prune stale payload catalog: %v", err)
	}
	e.logFn("engine: updated payload catalog (%d entries)", len(entries))
}

// ── Outbound messaging ──────────────────────────────────────────────

// SetSendFunc sets the function used to publish protocol envelopes.
func (e *Engine) SetSendFunc(fn func(*protocol.Envelope) error) {
	e.sendFn = fn
}

// SetKafkaReconnectFunc sets the function to reconnect the Kafka client
// after broker configuration changes at runtime.
func (e *Engine) SetKafkaReconnectFunc(fn func() error) {
	e.kafkaReconnFn = fn
}

// ReconnectKafka triggers a Kafka client reconnection using the current config.
func (e *Engine) ReconnectKafka() error {
	if e.kafkaReconnFn == nil {
		return fmt.Errorf("kafka reconnect not configured")
	}
	return e.kafkaReconnFn()
}

// SendEnvelope publishes a protocol envelope via the configured send function.
func (e *Engine) SendEnvelope(env *protocol.Envelope) error {
	if e.sendFn == nil {
		return fmt.Errorf("send function not configured (messaging not connected)")
	}
	return e.sendFn(env)
}
