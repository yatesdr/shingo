package engine

import (
	"log"
	"time"

	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/messaging"
	"shingocore/nodestate"
	"shingocore/rds"
	"shingocore/store"
)

type LogFunc func(format string, args ...any)

type Config struct {
	AppConfig  *config.Config
	ConfigPath string
	DB         *store.DB
	RDSClient  *rds.Client
	NodeState  *nodestate.Manager
	MsgClient  *messaging.Client
	LogFunc    LogFunc
	Debug      bool
}

type Engine struct {
	cfg          *config.Config
	configPath   string
	db           *store.DB
	rdsClient    *rds.Client
	nodeState    *nodestate.Manager
	msgClient    *messaging.Client
	dispatcher   *dispatch.Dispatcher
	poller       *rds.Poller
	Events       *EventBus
	logFn        LogFunc
	stopChan     chan struct{}
	rdsConnected bool
	msgConnected bool
}

func New(c Config) *Engine {
	logFn := c.LogFunc
	if logFn == nil {
		logFn = log.Printf
	}
	return &Engine{
		cfg:        c.AppConfig,
		configPath: c.ConfigPath,
		db:         c.DB,
		rdsClient:  c.RDSClient,
		nodeState:  c.NodeState,
		msgClient:  c.MsgClient,
		Events:     NewEventBus(),
		logFn:      logFn,
		stopChan:   make(chan struct{}),
	}
}

func (e *Engine) Start() {
	// Create emitter adapters
	de := &dispatchEmitter{bus: e.Events}
	pe := &pollerEmitter{bus: e.Events}

	// Create dispatcher
	e.dispatcher = dispatch.NewDispatcher(
		e.db,
		e.rdsClient,
		de,
		e.cfg.FactoryID,
		e.cfg.Messaging.DispatchTopicPrefix,
	)

	// Create poller
	e.poller = rds.NewPoller(
		e.rdsClient,
		pe,
		&orderResolver{db: e.db},
		e.cfg.RDS.PollInterval,
	)

	// Wire event handlers
	e.wireEventHandlers()

	// Load active RDS orders into poller
	e.loadActiveOrders()

	// Start poller
	e.poller.Start()

	// Emit initial connection status
	e.checkConnectionStatus()

	// Start periodic connection health check
	go e.connectionHealthLoop()

	e.logFn("engine: started")
}

func (e *Engine) Stop() {
	select {
	case e.stopChan <- struct{}{}:
	default:
	}
	if e.poller != nil {
		e.poller.Stop()
	}
	e.logFn("engine: stopped")
}

// Accessors
func (e *Engine) DB() *store.DB                  { return e.db }
func (e *Engine) AppConfig() *config.Config      { return e.cfg }
func (e *Engine) ConfigPath() string             { return e.configPath }
func (e *Engine) Dispatcher() *dispatch.Dispatcher { return e.dispatcher }
func (e *Engine) NodeState() *nodestate.Manager   { return e.nodeState }
func (e *Engine) Poller() *rds.Poller             { return e.poller }
func (e *Engine) RDSClient() *rds.Client          { return e.rdsClient }
func (e *Engine) MsgClient() *messaging.Client    { return e.msgClient }

func (e *Engine) checkConnectionStatus() {
	// RDS
	if _, err := e.rdsClient.Ping(); err == nil {
		if !e.rdsConnected {
			e.rdsConnected = true
			e.Events.Emit(Event{Type: EventRDSConnected, Payload: ConnectionEvent{Detail: "RDS Core connected"}})
		}
	} else {
		if e.rdsConnected {
			e.rdsConnected = false
			e.Events.Emit(Event{Type: EventRDSDisconnected, Payload: ConnectionEvent{Detail: err.Error()}})
		}
	}

	// Messaging
	if e.msgClient.IsConnected() {
		if !e.msgConnected {
			e.msgConnected = true
			e.Events.Emit(Event{Type: EventMessagingConnected, Payload: ConnectionEvent{Detail: "messaging connected"}})
		}
	} else {
		if e.msgConnected {
			e.msgConnected = false
			e.Events.Emit(Event{Type: EventMessagingDisconnected, Payload: ConnectionEvent{Detail: "messaging disconnected"}})
		}
	}
}

func (e *Engine) connectionHealthLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			e.checkConnectionStatus()
		}
	}
}

func (e *Engine) loadActiveOrders() {
	ids, err := e.db.ListDispatchedRDSOrderIDs()
	if err != nil {
		e.logFn("engine: load active orders: %v", err)
		return
	}
	for _, id := range ids {
		e.poller.Track(id)
	}
	if len(ids) > 0 {
		e.logFn("engine: loaded %d active RDS orders into poller", len(ids))
	}
}

// ReconfigureRDS applies RDS config changes live.
func (e *Engine) ReconfigureRDS() {
	e.rdsClient.Reconfigure(e.cfg.RDS.BaseURL, e.cfg.RDS.Timeout)
	e.logFn("engine: RDS reconfigured (%s)", e.cfg.RDS.BaseURL)
	e.checkConnectionStatus()
}

// ReconfigureMessaging reconnects messaging with current config.
func (e *Engine) ReconfigureMessaging() {
	if err := e.msgClient.Reconfigure(&e.cfg.Messaging); err != nil {
		e.logFn("engine: messaging reconfigure error: %v", err)
	} else {
		e.logFn("engine: messaging reconfigured (%s)", e.cfg.Messaging.Backend)
	}
	e.checkConnectionStatus()
}

// orderResolver implements rds.OrderIDResolver.
type orderResolver struct {
	db *store.DB
}

func (r *orderResolver) ResolveRDSOrderID(rdsOrderID string) (int64, error) {
	order, err := r.db.GetOrderByRDSID(rdsOrderID)
	if err != nil {
		return 0, err
	}
	return order.ID, nil
}
