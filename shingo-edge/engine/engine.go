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
	// catidMon tracks each press's live WarLink CATID_01 (part identity),
	// debounced. guardCatidMismatch reads it to block outgoing-style relief
	// when the live part diverges from the active style's expected_catid (A5,
	// Hopkinsville 2026-07-23). Nil until Start() (and in test fixtures that
	// build Engine directly) — the guard nil-checks and stays inert.
	catidMon *catidMonitor
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
	// Phase 6.5 split that into ServiceAccess + EngineOrchestration
	// (embeds ServiceAccess). Measured 2026-08-19: 19 and 71, the latter
	// adding 52 verbs of its own — the parenthetical here read "16" and
	// "35 verbs" from the split until then. www's width test is the
	// authority on both numbers; this comment is not.
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

	coreClient        *CoreClient
	coreNodes         map[string]protocol.NodeInfo
	coreNodesMu       sync.RWMutex
	payloadBinTypes   []protocol.PayloadBinTypeInfo
	payloadBinTypesMu sync.RWMutex
	nodeSyncFn        func()
	catalogSyncFn     func()
	sendFn            func(*protocol.Envelope) error
	kafkaReconnFn     func() error

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

	// strandedAlarms holds the current parked-ticks alarm detail per core node
	// name (map[string]string), maintained by strandedMonitor. The operator-
	// station view reads it via StrandedAlarmDetail so the tile renders the chip
	// on load AND on the SSE-driven refresh. Empty/absent = no active alarm.
	strandedAlarms sync.Map

	// loaderResv serializes the count→fire reservation per loader so concurrent
	// writers (an HTTP RequestEmptyBin vs the push sweep) can't both read the
	// same in-flight count and both fire empties —
	// the never-2N invariant. map[loaderID]*sync.Mutex, keyed from day one (no
	// global lock). NO transaction: see withLoaderBudget and
	// FINAL-ADJUDICATION Q1 (monotonicity + non-tx-pure CreateRetrieveOrder) —
	// shingo-library/archive/bin-loader-multiwindow-reviews-2026-06-12/FINAL-ADJUDICATION.md.
	loaderResv sync.Map

	// loaderStore is the consumer-defined resolver for loaders, backed by the
	// Core-owned aggregate (the synced core_loaders cache), refreshed on each
	// node-list sync.
	loaderStore LoaderStore

	// The park-and-replay machinery that used to live here closed a startup race:
	// a below-threshold signal arriving before the first node-list sync could not
	// resolve a loader, so it was held and replayed once the cache warmed. It is
	// gone with the signal — Core decides replenishment now and reads its own
	// tables, which are never cold in the way the Edge's cache was. That burst
	// shape appeared in the 2026-07-31 over-ordering incident; deleting it is the
	// durable fix, not a tidy-up.

	kafkaConnFn func() bool

	// homeConsolidations tracks pending two-order consolidation sequences
	// initiated by ClearLoaderHome. Key = Order A's UUID. When Order A's robot
	// picks up the empty carrier (home is now clear), HandleBinPickedUp fires
	// Order B (buffer partial → home). Protected by homeConsolidationsMu.
	homeConsolidations   map[string]homeConsolidation
	homeConsolidationsMu sync.Mutex

	// marketPullbacks tracks pull-from-market orders so the delivery handler
	// can auto-clear the bin when it arrives at the loader window.
	// Key = order UUID, value = Edge process node ID of the loader window.
	marketPullbacks   map[string]int64
	marketPullbacksMu sync.Mutex

	// pendingSiblingRelease records a two-robot swap leg whose consolidated
	// RELEASE was deferred by ReleaseStagedOrders because Core would have
	// refused it (still queued/sourcing/dispatched/acknowledged) WHILE its
	// sibling was released on the same operator click. When the deferred leg
	// later reaches staged, handleSiblingReleaseRefire fires the release the
	// operator already intended — the targeted revival of the auto-release-on-
	// staged coordination removed 2026-04-27 (hop A4-ii, 2026-07-23). Key =
	// deferred order id, value = the disposition to re-fire with. In-memory
	// only: on Edge restart the entry is lost and the operator re-taps RELEASE
	// (ComputeSwapReady keeps the button, P3-C3) — no timer, no reaper, and
	// nothing here ever cancels or re-plans an order. Lazy-initialised under
	// pendingSiblingReleaseMu so test fixtures that build Engine directly work.
	pendingSiblingRelease   map[int64]ReleaseDisposition
	pendingSiblingReleaseMu sync.Mutex
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
	// Wire the parked-ticks alarm (P2-C7) onto the operator tile: BuildView reads
	// the live alarm map so the chip renders on load and on every refresh.
	e.stationService.SetStrandedResolver(e.StrandedAlarmDetail)
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
	e.homeConsolidations = make(map[string]homeConsolidation)
	e.marketPullbacks = make(map[string]int64)
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

	// Give the counters service the SAME emitter the PLC poll loop uses, so
	// an operator confirming a jump releases its delta down the identical
	// path a normal tick takes. The poll withholds jump deltas pending
	// confirmation (plc/manager.go); this is the other half of that gate.
	e.counterService.SetDeltaEmitter(plcEmit)

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

	// Parked-ticks monitor (P2-C7): watches every consume node for
	// pending_uop_delta growing across consecutive flush intervals while
	// unbound, and raises a named stranding alarm. Detection only — never
	// binds or moves anything.
	e.startStrandedMonitor()

	// Demand-episode reconciler: closes any open cell or changeover episode
	// whose precondition no longer holds, however it stopped holding. The six
	// notification close paths are the fast path; this is the floor under
	// them, because a claim can quietly stop existing and nothing fires when
	// it does. Edge's half of the split — Core sweeps threshold episodes.
	e.startDemandReconciler()

	// R1 shadow read-model: push per-consuming-node lineside on-hand to Core
	// every 60s so Core can shadow its replenishment ledger against Edge's
	// authoritative counts. Reporting only; Core decides off the ledger.
	e.startLinesideReporter()

	// PLC part-identity (CATID) monitor: tracks each counter-bound process's
	// live CATID_01 and feeds the A5 guard (guardCatidMismatch) + raises the
	// wrong-part alert. Independent of auto-cutover — runs for every process
	// with a counter binding. No-op when the plant publishes no CATID tag.
	e.startCatidMonitor()

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
// prunes any local entries that no longer exist in core's response — all in
// ONE transaction (the sync fires every 2 minutes; 57 separate implicit
// txns per sync held the edge's single SQLite connection ~41,000 times/day
// to write back rows that almost never change). The upsert itself is
// conditional on a real change, so an unchanged catalog writes nothing.
func (e *Engine) HandlePayloadCatalog(entries []protocol.CatalogPayloadInfo) {
	rows := make([]*catalog.CatalogEntry, 0, len(entries))
	for _, b := range entries {
		rows = append(rows, &catalog.CatalogEntry{
			ID: b.ID, Name: b.Name, Code: b.Code,
			Description: b.Description,
			UOPCapacity: b.UOPCapacity,
			CATID:       b.CATID,
		})
	}
	if err := e.db.SyncPayloadCatalog(rows); err != nil {
		log.Printf("engine: sync payload catalog: %v", err)
	}
	// Now that the catalog (and its CATIDs) is current, retire any expected_catid
	// stamp that merely duplicates the style's derived single CATID (the guard now
	// derives the set live from the claims' payloads).
	e.ClearRedundantExpectedCATIDs()
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
