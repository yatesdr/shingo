package engine

import (
	"fmt"
	"log"
	"sync"
	"time"

	"shingo/protocol/debuglog"
	"shingoedge/changeover"
	"shingoedge/config"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/store"

	"shingo/protocol"
)

// LogFunc is the logging callback signature.
type LogFunc func(format string, args ...any)

// DebugLogFunc is a nil-safe debug logging function. Call Log() on it
// directly — a nil DebugLogFunc is safe and does nothing.
// Defined in the engine package to avoid import cycles; subsystem packages
// that cannot import engine define their own equivalent (same signature).
type DebugLogFunc func(format string, args ...any)

// Log calls the debug log function if non-nil. Safe to call on a nil receiver.
func (fn DebugLogFunc) Log(format string, args ...any) {
	if fn != nil {
		fn(format, args...)
	}
}

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

	changeoverMu   sync.RWMutex
	changeoverMgrs map[int64]*changeover.Machine
	coEmit         *changeoverEmitter

	hourlyTracker  *HourlyTracker
	reconciliation *ReconciliationService
	coreSync       *CoreSyncService

	coreNodes     map[string]protocol.NodeInfo
	coreNodesMu   sync.RWMutex
	nodeSyncFn    func()
	catalogSyncFn func()
	sendFn        func(*protocol.Envelope) error
	kafkaReconnFn func() error

	Events   *EventBus
	stopChan chan struct{}
}

// Config holds the parameters needed to create an Engine.
type Config struct {
	AppConfig   *config.Config
	ConfigPath  string
	DB          *store.DB
	LogFunc     LogFunc
	DebugLogger *debuglog.Logger
}

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
		cfg:            c.AppConfig,
		configPath:     c.ConfigPath,
		db:             c.DB,
		logFn:          logFn,
		debugFn:        debugFn,
		debugLogger:    c.DebugLogger,
		Events:         NewEventBus(),
		stopChan:       make(chan struct{}),
		changeoverMgrs: make(map[int64]*changeover.Machine),
	}
	e.reconciliation = newReconciliationService(e.db)
	e.coreSync = newCoreSyncService(e)
	return e
}

// Start creates all managers, wires event handlers, and starts subsystems.
func (e *Engine) Start() {
	// Create subsystem emitter adapters
	plcEmit := &plcEmitter{bus: e.Events}
	orderEmit := &orderEmitter{bus: e.Events}
	e.coEmit = &changeoverEmitter{bus: e.Events}

	// Create managers
	e.plcMgr = plc.NewManager(e.db, e.cfg, plcEmit)
	e.orderMgr = orders.NewManager(e.db, orderEmit, e.cfg.StationID())

	// Wire debug logging to subsystems
	if e.debugLogger != nil {
		e.plcMgr.DebugLog = plc.DebugLogFunc(e.debugLogger.Func("plc"))
		e.orderMgr.DebugLog = orders.DebugLogFunc(e.debugLogger.Func("orders"))
	}
	e.hourlyTracker = NewHourlyTracker(e.db, e.cfg.Timezone)

	// Initialize changeover machines for all processes
	lines, err := e.db.ListProcesses()
	if err != nil {
		log.Printf("load processes for changeover: %v", err)
	}
	for _, line := range lines {
		m := changeover.NewMachine(e.db, e.coEmit, line.ID, line.Name)
		if e.debugLogger != nil {
			m.DebugLog = changeover.DebugLogFunc(e.debugLogger.Func("changeover"))
		}
		m.Restore()
		e.changeoverMgrs[line.ID] = m
	}

	// Wire the event chain
	e.wireEventHandlers()

	// Start WarLink poller and counter polling
	if e.cfg.WarLink.Enabled {
		e.plcMgr.StartWarLinkPoller()
	}
	e.plcMgr.StartPolling()

	// Scan produce slots for empty bin needs on startup
	e.scanProduceSlots()

	e.logFn("Engine started: namespace=%s line_id=%s lines=%d", e.cfg.Namespace, e.cfg.LineID, len(e.changeoverMgrs))
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

// ChangeoverMachine returns the changeover state machine for a specific line.
// If no machine exists for the line, one is lazily created.
func (e *Engine) ChangeoverMachine(lineID int64) *changeover.Machine {
	e.changeoverMu.RLock()
	m, ok := e.changeoverMgrs[lineID]
	e.changeoverMu.RUnlock()
	if ok {
		return m
	}

	// Lazy create
	line, err := e.db.GetProcess(lineID)
	if err != nil {
		return nil
	}
	e.changeoverMu.Lock()
	defer e.changeoverMu.Unlock()
	// Double-check after acquiring write lock
	if m, ok := e.changeoverMgrs[lineID]; ok {
		return m
	}
	m = changeover.NewMachine(e.db, e.coEmit, line.ID, line.Name)
	if e.debugLogger != nil {
		m.DebugLog = changeover.DebugLogFunc(e.debugLogger.Func("changeover"))
	}
	m.Restore()
	e.changeoverMgrs[lineID] = m
	return m
}

// ChangeoverMachines returns all changeover machines (for iteration).
func (e *Engine) ChangeoverMachines() map[int64]*changeover.Machine {
	e.changeoverMu.RLock()
	defer e.changeoverMu.RUnlock()
	cp := make(map[int64]*changeover.Machine, len(e.changeoverMgrs))
	for k, v := range e.changeoverMgrs {
		cp[k] = v
	}
	return cp
}

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

// HandlePayloadCatalog upserts payload catalog entries received from core.
func (e *Engine) HandlePayloadCatalog(entries []protocol.CatalogPayloadInfo) {
	for _, b := range entries {
		entry := &store.PayloadCatalogEntry{
			ID: b.ID, Name: b.Name, Code: b.Code,
			Description: b.Description,
			UOPCapacity: b.UOPCapacity,
		}
		if err := e.db.UpsertPayloadCatalog(entry); err != nil {
			log.Printf("engine: upsert payload catalog entry %s: %v", b.Name, err)
		}
	}
	e.logFn("engine: updated payload catalog (%d entries)", len(entries))
}

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
