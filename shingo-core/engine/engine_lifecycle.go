package engine

import (
	"context"
	"database/sql"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/dispatch/eta"
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

	// Create fulfillment scanner for queued orders. The scanner shares the ONE
	// SourceFinder seam with intake planning (same resolver + DB) so replay can't
	// drift its source scoping from intake.
	sourceFinder := dispatch.NewSourceFinder(e.db, resolver, e.debugLog)
	e.fulfillment = fulfillment.NewScanner(e.db, e.dispatcher, e.dispatcher.Lifecycle(), sourceFinder, e.binManifest, e.sendToEdge, e.failOrderAndEmit, e.logFn, e.debugLog)

	// Wire event handlers
	e.wireEventHandlers()

	// Load active vendor orders into tracker
	e.loadActiveOrders()

	// Recover pending restore-blockers listeners from the
	// pending_restocks table (v7). The in-memory restoreRegistry is
	// volatile; without this a Core restart between unbury completion
	// and bin pickup would strand blockers in shuffle slots forever.
	// Errors are logged but non-fatal — fresh-install DBs don't yet
	// have the table and that's fine on the no-restore-needed path.
	if err := e.dispatcher.RecoverPendingRestocks(); err != nil {
		e.logFn("engine: recover pending_restocks: %v", err)
	}

	// Recover pending lane-lock-extension listeners from the
	// pending_lane_extensions table (post-v7 cleanup). Same shape as
	// the restore-blockers recovery above — without it a Core restart
	// during the post-compound / pre-pickup window would lose the
	// listener and the lane stays held forever (or worse, becomes
	// orphaned with no listener to release it).
	if err := e.dispatcher.RecoverPendingLaneExtensions(); err != nil {
		e.logFn("engine: recover pending_lane_extensions: %v", err)
	}

	// Seed one full-plant dashboard per type if a type has none yet (refactor
	// #5), so the hub + floor kiosk are useful out of the box. Idempotent +
	// non-fatal; never clobbers operator-created boards.
	if n, err := e.dashboardService.SeedDefaultDashboards(); err != nil {
		e.logFn("engine: seed default dashboards: %v", err)
	} else if n > 0 {
		e.logFn("engine: seeded %d default full-plant dashboard(s)", n)
	}

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
	go e.reconciliation.Loop(e.stopChan, e.cfg.Staging.SweepInterval, e.cfg.Staging.AutoConfirmDelivered, e.cfg.Staging.AbandonStuck)

	// Start count-group runner if configured (no-op if no groups enabled).
	e.countGroupMu.Lock()
	if e.countGroup != nil {
		e.countGroup.Start()
		e.logFn("engine: count-group runner started")
	}
	e.countGroupMu.Unlock()

	// ETA medians cache — initial refresh + 10-min background refresh.
	// Errors are logged but non-fatal: a cold-start failure leaves the
	// cache empty, the in-transit OrderUpdate path falls back to the
	// global p70 (also empty on cold start) and then to the static
	// default. Plant runs as before, just without per-route ETAs until
	// the next refresh succeeds.
	if err := e.etaCache.Start(e.stopChan); err != nil {
		e.logFn("engine: eta cache initial refresh failed: %v (will retry in %s)", err, "10m")
	}
	e.backfillETAsForInTransitOrders()

	// UOP-threshold monitor: startup sweep + ongoing subscription via
	// wireEventHandlers. Ctx tied to stopChan so the sweep aborts on
	// shutdown.
	if e.thresholdMonitor != nil {
		monCtx, cancel := context.WithCancel(context.Background())
		go func() {
			<-e.stopChan
			cancel()
		}()
		e.thresholdMonitor.Run(monCtx)
	}

	e.logFn("engine: started")
}

// backfillETAsForInTransitOrders re-stamps ETAs for orders that are
// already in_transit (or staged) at boot. Without this, an order that
// was mid-delivery when Core restarted would lose its ETA pill on the
// HMI until it next transitioned status — by which point it would be
// delivered and the pill irrelevant anyway. The fix is to look up each
// order's last in_transit timestamp in order_history and ship a fresh
// OrderUpdate so Edge can re-populate the pill.
//
// Skips orders that are already overdue past the grace window — those
// would render as "running late" immediately and the operator already
// knows the system is degraded; a backwards-pointing ETA adds noise.
func (e *Engine) backfillETAsForInTransitOrders() {
	if e.etaCache == nil {
		return
	}
	const q = `
SELECT o.id, o.edge_uuid, o.station_id, o.source_node, o.delivery_node, o.status,
       (SELECT MAX(created_at) FROM order_history
            WHERE order_id = o.id AND status = 'in_transit') AS last_in_transit_at
FROM orders o
WHERE o.status IN ('in_transit', 'staged')
`
	rows, err := e.db.Query(q)
	if err != nil {
		e.logFn("engine: eta backfill query: %v", err)
		return
	}
	defer rows.Close()

	const grace = 60 * time.Second
	var sent int
	for rows.Next() {
		var (
			id                                                    int64
			edgeUUID, stationID, sourceNode, deliveryNode, status string
			inTransitAt                                           sql.NullTime
		)
		if err := rows.Scan(&id, &edgeUUID, &stationID, &sourceNode, &deliveryNode, &status, &inTransitAt); err != nil {
			e.logFn("engine: eta backfill scan: %v", err)
			continue
		}
		if !inTransitAt.Valid {
			continue
		}
		etaStr := eta.StampFrom(e.etaCache, sourceNode, deliveryNode, inTransitAt.Time, grace)
		if etaStr == "" {
			continue
		}
		if err := e.sendToEdge(protocol.TypeOrderUpdate, stationID, &protocol.OrderUpdate{
			OrderUUID: edgeUUID,
			Status:    status,
			Detail:    "eta restored on boot",
			ETA:       etaStr,
		}); err != nil {
			e.logFn("engine: eta backfill send for order %d (%s): %v", id, edgeUUID, err)
			continue
		}
		sent++
	}
	if sent > 0 {
		e.logFn("engine: backfilled ETA for %d in-transit orders on boot", sent)
	}
}

func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.stopChan) })
	if e.fulfillment != nil {
		e.fulfillment.Stop()
	}
	if e.tracker != nil {
		e.tracker.Stop()
	}
	e.countGroupMu.Lock()
	if e.countGroup != nil {
		e.countGroup.Stop()
	}
	e.countGroupMu.Unlock()
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
