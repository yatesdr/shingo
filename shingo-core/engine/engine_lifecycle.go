package engine

import (
	"time"

	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fulfillment"
)

// ── Lifecycle ───────────────────────────────────────────────────────
//
// Start wires the dispatcher, tracker, and fulfillment scanner,
// registers event handlers (see wiring*.go), loads active vendor
// orders into the tracker, and launches the background loops defined
// in engine_background.go / engine_connection.go. Stop closes the
// stop channel and halts each subsystem that owns a goroutine.

func (e *Engine) Start() {
	// Create emitter adapters
	de := &dispatchEmitter{bus: e.Events}
	pe := &pollerEmitter{bus: e.Events}

	// Create dispatcher with synthetic node resolver
	resolver := &dispatch.DefaultResolver{DB: e.db, DebugLog: e.debugLog}
	e.dispatcher = dispatch.NewDispatcher(
		e.db,
		e.fleet,
		de,
		e.cfg.Messaging.StationID,
		e.cfg.Messaging.DispatchTopic,
		resolver,
	)
	// Share the lane lock between dispatcher and resolver
	resolver.LaneLock = e.dispatcher.LaneLock()

	// Initialize tracker if backend supports it
	if tb, ok := e.fleet.(fleet.TrackingBackend); ok {
		tb.InitTracker(pe, &orderResolver{db: e.db})
		e.tracker = tb.Tracker()
	}

	// Create fulfillment scanner for queued orders
	e.fulfillment = fulfillment.NewScanner(e.db, e.dispatcher, resolver, e.sendToEdge, e.failOrderAndEmit, e.logFn, e.debugLog)

	// Wire event handlers
	e.wireEventHandlers()

	// Load active vendor orders into tracker
	e.loadActiveOrders()

	// Scan for any orders queued before restart
	go e.fulfillment.RunOnce()

	// Start periodic fulfillment sweep (60s safety net)
	e.fulfillment.StartPeriodicSweep(60 * time.Second)

	// Start tracker
	if e.tracker != nil {
		e.tracker.Start()
	}

	// Emit initial connection status
	e.checkConnectionStatus()

	// Start periodic connection health check
	go e.connectionHealthLoop()

	// Start robot status refresh loop (2s)
	go e.robotRefreshLoop()

	// Start staged bin expiry sweep
	go e.stagedBinSweepLoop()

	// Start periodic reconciliation logging and auto-confirm
	go e.reconciliation.Loop(e.stopChan, e.cfg.Staging.SweepInterval, e.cfg.Staging.AutoConfirmDelivered)

	// Start count-group runner if configured (no-op if no groups enabled).
	if e.countGroup != nil {
		e.countGroup.Start()
		e.logFn("engine: count-group runner started")
	}

	e.logFn("engine: started")
}

func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.stopChan) })
	if e.fulfillment != nil {
		e.fulfillment.Stop()
	}
	if e.tracker != nil {
		e.tracker.Stop()
	}
	if e.countGroup != nil {
		e.countGroup.Stop()
	}
	e.logFn("engine: stopped")
}

func (e *Engine) loadActiveOrders() {
	if e.tracker == nil {
		return
	}
	ids, err := e.db.ListDispatchedVendorOrderIDs()
	if err != nil {
		e.logFn("engine: load active orders: %v", err)
		return
	}
	for _, id := range ids {
		e.tracker.Track(id)
	}
	if len(ids) > 0 {
		e.logFn("engine: loaded %d active vendor orders into tracker", len(ids))
	}
}
