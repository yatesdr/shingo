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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/protocol/types"
	"shingo/shared/clock"
	"shingoedge/config"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/uop"
)

// ── Types & struct ──────────────────────────────────────────────────

// LogFunc is the logging callback signature.
type LogFunc = types.DebugLogFunc

// DebugLogFunc is a nil-safe debug logging function.
type DebugLogFunc = types.DebugLogFunc

// InventoryDeltaSink is the engine's view of the UOP mutator
// (shingoedge/uop.Mutator). Now aliased to uop.Sink which composes
// the segregated sub-interfaces (Ticker, SlotWriter, Capturer,
// Pickup, Boundary, Backfiller) plus the legacy four-method shim.
//
// Engine functions that only consume one slice (e.g., the PLC tick
// path) can take a uop.Ticker parameter directly rather than the
// full Sink. Test fakes can satisfy a single sub-interface when they
// only exercise one concern.
//
// nil-safe: callers in the PLC tick path and the release path guard
// every verb call with a nil check on Engine.inventoryDelta so
// tests / off-modes can leave the field unset.
type InventoryDeltaSink = uop.Sink

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
	// warlinkClient is the injected WarLink client (sim fake) carried from
	// Config.Warlink to the NewManager call in Start(). Nil → real HTTP client.
	warlinkClient plc.WarlinkClient

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
	preflightChecker  *service.PreflightChecker
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

	// inventoryDelta is the Phase 1 delta sink. Set by the composition
	// root via SetInventoryDeltaSink. Nil in test contexts that don't
	// care about delta emission; every call site nil-guards.
	inventoryDelta InventoryDeltaSink

	Events           *EventBus
	stopChan         chan struct{}
	startedAt        time.Time
	subscribersWired atomic.Bool
	// sweepingUnloaders / sweepingLoaders guard the startup push sweeps
	// against stacking: registration-ack spawns them as goroutines and a
	// re-register storm can fire overlapping sweeps that double-create through
	// the list-then-create dedup gap. One sweep of each kind at a time.
	sweepingUnloaders atomic.Bool
	sweepingLoaders   atomic.Bool
	// l1Burst is the per-delivery-node burst tripwire (PR-0 observability):
	// WARNs when too many loader/unloader in-bin orders land on one node in a
	// short window. Zero value is usable.
	l1Burst loaderBurstTracker

	// loaderResv serializes the count→fire reservation per loader so concurrent
	// writers (a Kafka demand signal vs an HTTP RequestEmptyBin, or the push
	// sweep) can't both read the same in-flight count and both fire empties —
	// the never-2N invariant. map[loaderID]*sync.Mutex, keyed from day one (no
	// global lock). NO transaction: see reserveLoaderEmpties and
	// FINAL-ADJUDICATION Q1 (monotonicity + non-tx-pure CreateRetrieveOrder).
	loaderResv sync.Map

	// loaderStore is the consumer-defined resolver for loaders, backed by the
	// Core-owned aggregate (the synced core_loaders cache), refreshed on each
	// node-list sync.
	loaderStore LoaderStore

	// loaderCacheWarmed + pendingThreshold close the startup race where a Core
	// LoopBelowThresholdSignal arrives before the first node-list sync populates the
	// loader cache: the signal can't resolve a loader and would be dropped. Until the
	// cache has synced once, such a signal is PARKED here and replayed the instant
	// SetCoreLoaders warms the cache, so no startup reorder is lost (hold-and-replay).
	loaderCacheWarmed atomic.Bool
	pendingThreshMu   sync.Mutex
	pendingThreshold  []*protocol.LoopBelowThresholdSignal

	kafkaConnFn func() bool
}

// Config holds the parameters needed to create an Engine.
type Config struct {
	AppConfig   *config.Config
	ConfigPath  string
	DB          *store.DB
	LogFunc     LogFunc
	DebugLogger *debuglog.Logger
	// Warlink, when non-nil, is injected into the PLC manager in place of the
	// default HTTP client (D3). Sim mode sets this to the fake WarLink client
	// (T3.1); production leaves it nil so NewManager builds the real client.
	Warlink plc.WarlinkClient
}

// ── Lifecycle ───────────────────────────────────────────────────────

// New creates a new Engine. Call Start() to initialize and wire subsystems.
func New(c Config) *Engine {
	logFn := c.LogFunc
	if logFn == nil {
		logFn = func(string, ...any) {}
	}
	var debugFn DebugLogFunc
	if c.DebugLogger != nil {
		debugFn = DebugLogFunc(c.DebugLogger.Func("engine"))
	}
	if debugFn == nil {
		debugFn = func(string, ...any) {}
	}
	e := &Engine{
		cfg:           c.AppConfig,
		configPath:    c.ConfigPath,
		db:            c.DB,
		logFn:         logFn,
		debugFn:       debugFn,
		debugLogger:   c.DebugLogger,
		warlinkClient: c.Warlink,
		Events:        NewEventBus(),
		stopChan:      make(chan struct{}),
	}
	e.coreClient = NewCoreClient(c.AppConfig.CoreAPI)
	e.reconciliation = newReconciliationService(e.db)
	e.coreSync = newCoreSyncService(e)
	e.stationService = service.NewStationService(e.db)
	// Wire the operator view to the SAME flag-selected loader resolver the runtime
	// uses, so the board's window-group membership and the empties it spreads can
	// never disagree (multi-window C4b). Lazy via loaders() so the flag dual is
	// honoured; the not-found sentinel is mapped to a clean miss for the view.
	e.stationService.SetLoaderResolver(stationLoaderResolver{e})
	e.changeoverService = service.NewChangeoverService(e.db)
	e.adminService = service.NewAdminService(e.db)
	e.processService = service.NewProcessService(e.db)
	e.styleService = service.NewStyleService(e.db)
	e.shiftService = service.NewShiftService(e.db)
	e.counterService = service.NewCounterService(e.db)
	e.catalogService = service.NewCatalogService(e.db)
	e.orderService = service.NewOrderService(e.db)
	e.preflightChecker = service.NewPreflightChecker(e.db, e.coreClient, e.cfg.StationID())
	e.loaderStore = newLoaderStore(e)
	return e
}

// SetInventoryDeltaSink installs the Phase 1 delta reporter. Called by
// the composition root (cmd/shingoedge/main.go) after both the engine
// and the reporter exist. Idempotent; the latest sink wins. Nil
// disables delta emission — useful in tests that don't care about
// Phase 1 plumbing.
func (e *Engine) SetInventoryDeltaSink(s InventoryDeltaSink) {
	e.inventoryDelta = s
}

// Start creates all managers, wires event handlers, and starts subsystems.
func (e *Engine) Start() {
	// Create subsystem emitter adapters
	plcEmit := &plcEmitter{bus: e.Events}
	orderEmit := &orderEmitter{bus: e.Events}

	// Create managers
	e.plcMgr = plc.NewManager(e.db, e.cfg, plcEmit, e.warlinkClient)
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

	// PLC-driven changeover-completion monitor: subscribes to each
	// auto-cutover-enabled process's Changeover_Active tag and fires
	// CompleteProcessProductionCutover on debounced falling edges. No-op
	// when no processes have the flag set.
	e.startCutoverMonitor()

	e.startedAt = time.Now()
	e.logFn("Engine started: namespace=%s line_id=%s", e.cfg.Namespace, e.cfg.LineID)
}

// ── Accessors ───────────────────────────────────────────────────────

// Uptime returns the number of seconds since the engine started.
func (e *Engine) Uptime() int64 {
	return int64(time.Since(e.startedAt).Seconds())
}

// StartedAt returns the engine's startup wall-clock time. Used by
// /status for process_start_time.
func (e *Engine) StartedAt() time.Time {
	return e.startedAt
}

// SubscribersWired returns true once setupKafkaSubscribers has run
// successfully — meaning Edge is hooked up to receive inbound Kafka
// messages (orders, demand, stale notifications). Pre-wire (or if
// Kafka never connected) returns false; /status surfaces this so
// operators can see the deaf-but-running mode.
func (e *Engine) SubscribersWired() bool {
	return e.subscribersWired.Load()
}

// MarkSubscribersWired is called by setupKafkaSubscribers when it
// finishes successfully.
func (e *Engine) MarkSubscribersWired() {
	e.subscribersWired.Store(true)
}

// KafkaConnected returns true if the messaging client is currently
// connected to Kafka. Returns false if the client is nil (test
// fixtures) or if the connection has not been established.
func (e *Engine) KafkaConnected() bool {
	if e.kafkaConnFn == nil {
		return false
	}
	return e.kafkaConnFn()
}

// SetKafkaConnFunc injects the messaging client's IsConnected
// closure so the engine can report it via /status without taking
// a hard dependency on the messaging package.
func (e *Engine) SetKafkaConnFunc(fn func() bool) {
	e.kafkaConnFn = fn
}

// StationID returns the station identifier from config.
func (e *Engine) StationID() string {
	return e.cfg.StationID()
}

// CountPendingOutbox returns the count of un-sent outbox messages.
// Surfaced via /status — a steadily growing depth is the
// operational signal that Kafka or Core is unreachable.
func (e *Engine) CountPendingOutbox() (int, error) {
	return e.db.CountPendingOutbox()
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
	// In sim mode the PLC manager holds an injected fake WarLink client; a
	// single visit to the HMI PLC-settings page would otherwise swap it for a
	// real HTTP client and silently kill the production sim (bug F2). Ignore.
	if e.cfg.Sim.Enabled {
		e.logFn("[sim] ignoring WarLink config apply — keeping the injected sim client")
		return
	}
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

// WarlinkClient returns the injected Warlink client (real or sim fake).
func (e *Engine) WarlinkClient() plc.WarlinkClient { return e.warlinkClient }

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
//
// Core qualifies a group-child node's name as "Group.Child" when it builds the
// node list (core_data_service.go), purely for display uniqueness in the edge
// pickers. The runtime, however, keys on the BARE child name everywhere that
// matters — loader windows (BuildLoaderInfos → node.Name), the orders a claim
// emits (consume_plan: SimpleDest = claim.CoreNodeName), and Core's own node
// resolution. So a node picked under its qualified name matches no loader window
// and emits an order Core can't resolve (the unloader-board-blank bug). Trim to
// the bare name here, at the single ingestion point, so every picker downstream
// stores the identity the runtime matches. Collision-safe: if two qualified
// names reduce to the same bare name, the later keeps its qualified form so no
// node is silently dropped.
func (e *Engine) SetCoreNodes(nodes []protocol.NodeInfo) {
	e.coreNodesMu.Lock()
	e.coreNodes = make(map[string]protocol.NodeInfo, len(nodes))
	normalized := make([]protocol.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		if bare := bareNodeName(n.Name); bare != n.Name {
			if _, taken := e.coreNodes[bare]; !taken {
				n.Name = bare
			}
		}
		e.coreNodes[n.Name] = n
		normalized = append(normalized, n)
	}
	e.coreNodesMu.Unlock()

	e.Events.Emit(Event{
		Type:      EventCoreNodesUpdated,
		Timestamp: clock.Now(),
		Payload:   CoreNodesUpdatedEvent{Nodes: normalized},
	})
}

// bareNodeName strips Core's display-only "Group." prefix from a group-child
// node name, returning the bare child name the runtime keys on. Node names carry
// no dot themselves, so the segment after the last dot is the child name.
func bareNodeName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
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
