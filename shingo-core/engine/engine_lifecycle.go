package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/shared/planttime"
	"shingocore/cms/client"
	"shingocore/cms/poster"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/dispatch/eta"
	"shingocore/fleet"
	"shingocore/fulfillment"
	"shingocore/material"
	"shingocore/notify"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// ── Lifecycle ───────────────────────────────────────────────────────
//
// Start wires the dispatcher, tracker, and fulfillment scanner,
// registers event handlers (see wiring*.go), loads active vendor
// orders into the tracker, and launches the background loops defined
// in engine_background.go / engine_connection.go. Stop closes the
// stop channel and halts each subsystem that owns a goroutine.

func (e *Engine) Start() {
	// Every ask open now was left by a Core that died inside a create: nothing
	// in this process can have opened one yet (the doors subscribe after Start,
	// the scanner starts below). Named first (reconciliation_boot_asks.go).
	e.reconciliation.TakeBootAsks()

	e.startFulfillment(e.startDispatch())

	// Wire event handlers, and start the goroutine the ones that acquire hand
	// their work to.
	e.loops.spawn(func() { e.after.run(e.stopChan) })
	e.wireEventHandlers()

	// Teach the fleet backend the plant's one-bin-per-node invariant. Only the
	// simulator implements PositionGated — a real fleet gets this from physics.
	// Without it the sim completes blocks on a timer and "delivers" onto occupied
	// positions, which cannot happen in a plant and makes Core evict good bins as
	// stale ghosts (2026-07-13 press-swap chase).
	if pg, ok := e.fleet.(fleet.PositionGated); ok {
		pg.SetPositionGate(e)
	}

	// Start the fleet driver goroutine AFTER event handlers are wired.
	// The sim driver emits FINISHED events immediately; without this gate
	// handleVendorStatusChanged is never called and bins enter _TRANSIT
	// permanently (Item 3, sim startup race). Backends without a
	// deferred-start driver simply don't implement this interface.
	if ds, ok := e.fleet.(fleet.DriverStarter); ok {
		if err := ds.StartDriver(context.Background()); err != nil {
			e.logFn("engine: start driver: %v", err)
		}
	}

	// Load active vendor orders into tracker
	e.loadActiveOrders()

	// One-shot: cancel any leftover reshuffle_restore housekeeping orders from
	// the retired restore-blockers subsystem (blockers lie now). No-op on a clean
	// DB and idempotent across restarts. Non-fatal.
	if n, err := e.db.RetireReshuffleRestoreOrders(); err != nil {
		e.logFn("engine: retire reshuffle_restore orders: %v", err)
	} else if n > 0 {
		e.logFn("engine: retired %d leftover reshuffle_restore order(s)", n)
	}

	// Boot-time reshuffle liveness backstop: re-drive any compound (reshuffle) parent
	// left in `reshuffling` with all children terminal. A crash between the last
	// child's terminal transition and AdvanceCompoundOrder — or a cancelled child (no
	// child→parent event arm) — would otherwise strand it forever. The periodic
	// reconcile Loop repeats this; running it once at boot recovers pre-restart
	// strands immediately. Non-fatal.
	if n, err := e.reconciliation.AdvanceStuckReshuffleParents(); err != nil {
		e.logFn("engine: boot advance stuck reshuffle parents: %v", err)
	} else if n > 0 {
		e.logFn("engine: boot re-drove %d stuck reshuffle parents", n)
	}

	// Seed one full-plant dashboard per type if a type has none yet (refactor
	// #5), so the hub + floor kiosk are useful out of the box. Idempotent +
	// non-fatal; never clobbers operator-created boards.
	if n, err := e.dashboardService.SeedDefaultDashboards(); err != nil {
		e.logFn("engine: seed default dashboards: %v", err)
	} else if n > 0 {
		e.logFn("engine: seeded %d default full-plant dashboard(s)", n)
	}

	// Setups saved half-finished or before a rule, named at boot (reportSetups).
	e.reportSetups()

	// Two-stage unloaders: pairs re-derived, and pull-mode carts moved on, now
	// rather than at the next edit or event; and every draining unloader's
	// drain run once (startTwoStage).
	e.startTwoStage()

	// Scan for any orders queued before restart
	e.startBootScan()

	// Start periodic fulfillment sweep (60s safety net)
	e.fulfillment.StartPeriodicSweep(60 * time.Second)

	// The lane liveness floor — the same safety net for the two populations the
	// fulfillment sweep does not cover: robots dwelling at a lane's mark, and
	// compound legs not yet handed to the fleet. Started beside it because it is
	// the same shape at the same cadence over a different set (F-22).
	e.loops.spawn(e.laneLivenessFloorLoop)

	// Start tracker
	if e.tracker != nil {
		e.tracker.Start()
	}

	// The robot-route index from the scene already stored, before the first
	// connection check below can start a scene sync: a sync's own rebuild then
	// always lands after this one (rebuildPointIndex).
	e.rebuildPointIndex()

	// Emit initial connection status
	e.checkConnectionStatus()

	// Start periodic connection health check
	e.loops.spawn(e.connectionHealthLoop)

	// Start robot status refresh loop (2s), and the loops that read its poll.
	e.startRobotPollLoops()

	// Start the orphaned-claim sweep, and the chase of orders shingo ended that
	// the fleet may still be running (fleet_terminate_chase.go).
	e.startClaimAndTerminateSweeps()

	// Map + scene sync gates. Deliberately NO boot pass, unlike the confidence
	// roll-up: both gates read the robot cache, which robotRefreshLoop above
	// fills on its 2-second tick, so a pass at boot would run against an empty
	// cache and decide nothing. The first tick five minutes in is the earliest
	// the answer means anything.
	e.loops.spawn(e.mapSyncLoop)

	// Demand-episode reconciler: the correctness floor under the notification
	// close paths. Started AFTER the threshold monitor is constructed (New
	// builds it) but independently of its startup sweep — the reconciler reads
	// the database, not the monitor's caches, so it does not need the sweep to
	// have finished to be correct. If it happens to run first it finds the
	// bindings in demand_registry exactly as they are.
	e.startDemandReconciler()

	// Start periodic reconciliation logging and auto-confirm.
	e.startReconciliationLoop()

	// ETA medians cache — initial refresh + 10-min background refresh.
	// Errors are logged but non-fatal: a cold-start failure leaves the
	// cache empty, the in-transit OrderUpdate path falls back to the
	// global p70 (also empty on cold start) and then to the static
	// default. Plant runs as before, just without per-route ETAs until
	// the next refresh succeeds.
	if err := e.etaCache.Start(); err != nil {
		e.logFn("engine: eta cache initial refresh failed: %v (will retry in %s)", err, "10m")
	}
	e.loops.spawn(func() { e.etaCache.Run(e.stopChan) })
	e.backfillETAsForInTransitOrders()

	// UOP-threshold monitor: startup sweep + ongoing subscription via
	// wireEventHandlers. Ctx tied to stopChan so the sweep aborts on
	// shutdown.
	if e.thresholdMonitor != nil {
		monCtx, cancel := context.WithCancel(context.Background())
		e.loops.spawn(func() {
			<-e.stopChan
			cancel()
		})
		e.loops.spawn(func() { e.thresholdMonitor.Run(monCtx) })
	}

	// Sourceability monitor: startup full recompute + periodic safety net,
	// with change-driven debounced recomputes via the bus subscriptions.
	if e.sourceabilityMonitor != nil {
		srcCtx, cancel := context.WithCancel(context.Background())
		e.loops.spawn(func() {
			<-e.stopChan
			cancel()
		})
		e.sourceabilityMonitor.Run(srcCtx)
	}

	// The level keeper. Live-capable and gated ONLY by configuration: a plant with
	// no group declaring a level gives it nothing to do, so starting it
	// unconditionally starts nothing.
	//
	// LAST, and after the dispatcher exists: the keeper mints orders through
	// AdmitCoreAsk, and Engine.New leaves e.dispatcher nil (Start constructs it
	// above). A keeper ticking before that would panic on its first gap.
	if e.keeper != nil {
		mntCtx, cancel := context.WithCancel(context.Background())
		e.loops.spawn(func() {
			<-e.stopChan
			cancel()
		})
		e.loops.spawn(func() { e.keeper.Run(mntCtx) })
	}

	// The CMS poster's LOOP. The poster itself was built in New; only its
	// goroutine starts here, and it starts here rather than in New because
	// wireEventHandlers has to have run first — a drain that began before the
	// subscription existed would empty the queue while the first arrivals went
	// nowhere.
	e.runCMSPoster()

	e.logFn("engine: started")
}

// startRobotPollLoops starts the 2-second robot poll and the two loops that
// act on what it last saw: the air-knife cleaning tick and the hourly lidar
// round. Both read e.latestPolledRobots and need the dispatcher, which Start
// has built by the time this runs; the cleaning tick's first pass is a minute
// out and the lidar round waits for HH:05 UTC, so the poll is well under way
// before either acts. Neither does anything until the plant has a cleaning
// mission set up (cleaning) or a fleet that can relay a laser frame (lidar),
// and each says so once.
func (e *Engine) startRobotPollLoops() {
	e.loops.spawn(e.robotRefreshLoop)
	e.loops.spawn(e.cleaningLoop)
	e.loops.spawn(e.lidarHoursLoop)
}

// newCMSPoster builds the middleware poster when the site has one, and returns
// nil when it does not. Configuration is the gate: a site with no cms: block
// has no poster at all rather than a switched-off one.
//
// A FREE FUNCTION CALLED FROM New, not a method called from Start, because the
// field it fills is read without a lock from the event subscriber's goroutine
// and from HTTP handlers. Written before any of those exist, it needs no lock;
// written at the end of Start(), it needed one nobody had given it.
//
// The credentials were already validated at config load — a base_url without
// keys, or one that will not parse, refuses to boot — so a non-nil return means
// the integration can actually run.
// startFulfillment creates the fulfillment scanner for acquiring orders and
// hands it to the dispatcher as the doors' lock and their one acquire
// (dispatch/door.go, F1). The scanner shares the ONE SourceFinder seam with
// intake planning (same resolver + DB) so replay can't drift its source
// scoping from intake.
func (e *Engine) startFulfillment(resolver dispatch.NodeResolver) {
	sourceFinder := dispatch.NewSourceFinder(e.db, resolver, e.debugLog)
	e.fulfillment = fulfillment.NewScanner(e.db, e.dispatcher, e.dispatcher.Lifecycle(), sourceFinder, e.binManifest, e.sendToEdge, e.failOrderAndEmit, e.logFn, e.debugLog)
	e.dispatcher.SetAcquirer(e.fulfillment)
}

// startDispatch builds the dispatcher, with the synthetic node resolver, and the
// tracker when the backend tracks orders; it returns the resolver for the
// fulfillment scanner.
func (e *Engine) startDispatch() *dispatch.DefaultResolver {
	de := &dispatchEmitter{bus: e.Events, engine: e}
	pe := &pollerEmitter{bus: e.Events, audit: e.auditOrderNote}
	resolver := &dispatch.DefaultResolver{DB: e.db, DebugLog: e.debugLog}
	e.dispatcher = e.newDispatcher(de, resolver)
	if tb, ok := e.fleet.(fleet.TrackingBackend); ok {
		tb.InitTracker(pe, &orderResolver{db: e.db})
		e.tracker = tb.Tracker()
	}
	return resolver
}

// startBootScan scans, on its own goroutine, the orders queued before a restart,
// and closes bootScanned when the scan has finished.
func (e *Engine) startBootScan() {
	e.fulfillment.StartBootScan(func() { close(e.bootScanned) })
}

func newCMSPoster(cfg *config.Config, db *store.DB, logFn func(string, ...any)) *poster.Poster {
	if !cfg.CMS.Enabled() {
		return nil
	}
	return poster.New(db, client.New(cfg.CMS.Client()), cfg.CMS.Poster(), logFn)
}

// runCMSPoster starts the poster's drain loop and ties it to the engine's stop
// channel. No-op when the site has no poster.
func (e *Engine) runCMSPoster() {
	if e.cmsPoster == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.loops.spawn(func() {
		<-e.stopChan
		cancel()
	})
	e.loops.spawn(func() { e.cmsPoster.Run(ctx) })

	// An empty feed is a legitimate state before cutover, and it is
	// indistinguishable from a broken one without saying which this is.
	e.warnIfNoCMSBoundaries()
	e.logFn("engine: cms poster started against %s (poll %s, settle %s, max attempts %d)",
		e.cfg.CMS.BaseURL, e.cfg.CMS.PollInterval, e.cfg.CMS.SettleWindow, e.cfg.CMS.MaxAttempts)
}

// warnIfNoCMSBoundaries says so when the integration is on and nothing is
// tagged. Not fatal: a plant configures the endpoint before it tags the nodes,
// and that ordering is normal. But zero rows for that reason and zero rows
// because the feed is broken look identical from the outside, and only this
// line distinguishes them.
func (e *Engine) warnIfNoCMSBoundaries() {
	n, err := material.CountCMSBoundaries(e.db.DB)
	if err != nil {
		e.logFn("engine: cms boundary census: %v", err)
		return
	}
	if n == 0 {
		e.logFn("engine: cms is configured but NO node carries a %s property — "+
			"nothing will ever be posted until the boundary nodes are tagged with their "+
			"storeroom codes", material.CMSStoreroomProperty)
	}
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
	// The order's requester: its station, or the Core actor that asked (each was
	// sent its update before v185 split the actors out of the station column).
	q := `
SELECT o.id, o.edge_uuid, ` + orders.RequesterSQL("o") + `, o.source_node, o.delivery_node,
       (SELECT MAX(created_at) FROM order_history
            WHERE order_id = o.id AND status = 'in_transit') AS last_in_transit_at
FROM orders o
WHERE o.status IN ('in_transit', 'staged') AND NOT o.station_unaware
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
			id                                            int64
			edgeUUID, stationID, sourceNode, deliveryNode string
			inTransitAt                                   sql.NullTime
		)
		if err := rows.Scan(&id, &edgeUUID, &stationID, &sourceNode, &deliveryNode, &inTransitAt); err != nil {
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
		// THE ETA ONLY, NO STATUS. The order's status reached its station from
		// the write that set it (store queueTellTx); this restores a pill a Core
		// restart lost, and claims nothing about the status. An empty status is
		// no status claim: an Edge applies the ETA and leaves the status as it
		// is. It once carried the row's status, which made the backfill the only
		// thing that ever told a station a Core-side release happened.
		if err := e.sendToEdge(protocol.TypeOrderUpdate, stationID, &protocol.OrderUpdate{
			OrderUUID: edgeUUID,
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

// engineStopDrainLimit is the one deadline over every wait in Stop. A safety
// net, as the RDS poller's stopDrainLimit is: each loop returns on stopChan and
// each hand-off is one pass or one statement chain, so a wait this long means
// one is wedged, and returning is better than holding the process's shutdown
// forever. Pinned by TestEngineStop_OneDeadlineOverEveryWait.
//
// It has to fit inside the service's stop timeout. The unit install-core.sh
// installs (shingo-core/deploy/shingo-core.service) sets no TimeoutStopSec, so
// systemd's DefaultTimeoutStopSec applies: 90 s unless the host changes it.
// The same shutdown also spends up to 5 s in the HTTP server's (awaitShutdown),
// and waits on the outbox drainer and the Kafka client's Close, which this
// limit does not cover.
const engineStopDrainLimit = 30 * time.Second

// Stop ends the engine: it takes no new work, what is running finishes, what
// is still queued is dropped (afterQueue.run says what a restart redoes), and
// it returns by engineStopDrainLimit whatever is still running.
func (e *Engine) Stop() { e.stopBounded(engineStopDrainLimit) }

// stopBounded is Stop with its limit given.
func (e *Engine) stopBounded(limit time.Duration) {
	e.stopOnce.Do(func() { close(e.stopChan) })
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		e.stopAndWait()
	}()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	select {
	case <-drained:
	case <-deadline.C:
		e.logFn("engine: stop: work was still running after %s; not waiting longer", limit)
	}
	e.logFn("engine: stopped")
}

// stopAndWait stops each subsystem and waits for what it had running, after
// stopChan has closed (every loop ends and none starts another turn), in this
// order:
//
//  1. The work in hand that reads the database is joined, all of it at once
//     under one bound (joinStopped). Stop holds no lock while it waits, so
//     whatever waits for the scan lock gets it when the running pass ends at
//     its next order boundary.
//  2. The tracker stops, after the joins, so a dispatch made by the order in
//     hand is still tracked.
//  3. The engine's tracked group, last.
//
// The joins bound themselves; the waits after them do not, and stopBounded's
// limit covers them all.
func (e *Engine) stopAndWait() {
	e.joinStopped()
	if e.tracker != nil {
		e.tracker.Stop()
	}
	if e.sourceabilityMonitor != nil {
		e.sourceabilityMonitor.stop()
	}
	// Last: every loop has seen stopChan close, and a hand-off still running
	// (an after-queue item, a scan a subscriber started) finishes here, before
	// the caller closes the database (Engine.loops).
	e.loops.stop()
}

// joinStopped waits, for all of them at once and never past the bound, for the
// scanner's pass and boot scan, the window pulls in hand and the after-queue's
// work in hand. The last two wait for the scan lock (their acquires), so they
// are not waited for ahead of the scanner's stop.
func (e *Engine) joinStopped() {
	var joined sync.WaitGroup
	if e.fulfillment != nil {
		joined.Go(e.fulfillment.Stop) // bounds itself, by the same limit
	}
	joined.Go(func() { e.joinWithin("the window pulls", e.stage2.stop) })
	if e.after != nil {
		joined.Go(func() { e.joinWithin("the after-emit queue", e.after.join) })
	}
	joined.Wait()
}

// joinWithin waits for wait to return, or logs that stop gave up on what.
func (e *Engine) joinWithin(what string, wait func()) {
	limit := e.stopWait
	if limit <= 0 {
		limit = fulfillment.StopJoinLimit
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		e.logFn("engine: stop gave up waiting for %s after %s; shutting down without it", what, limit)
	}
}

// rearmReleasedOrders re-arms, on the fleet's tracker, every open order the
// station released past a wait (order 112 across a restart): a Core restart
// in the seconds after a release would otherwise stage it at its next wait
// while the robot still stands at the old one. One fleet read per such order,
// at boot only.
func (e *Engine) rearmReleasedOrders() {
	r, ok := e.fleet.(fleet.ReleasedOrderRearmer)
	if !ok {
		return
	}
	ids, err := e.db.ListReleasedOpenVendorOrderIDs()
	if err != nil {
		e.logFn("engine: list released open orders: %v", err)
		return
	}
	for _, id := range ids {
		if err := r.RearmReleased(id); err != nil {
			e.logFn("engine: %v — tracked as a new order; its next WAITING read stages it", err)
		}
	}
	if len(ids) > 0 {
		e.logFn("engine: re-armed %d released open order(s) on the tracker", len(ids))
	}
}

func (e *Engine) loadActiveOrders() {
	if e.tracker == nil {
		return
	}
	ids, err := e.db.ListTrackedVendorOrderIDs()
	if err != nil {
		e.logFn("engine: load active orders: %v", err)
		return
	}
	for _, id := range ids {
		e.tracker.Track(id)
	}
	if len(ids) == 0 {
		return
	}
	e.rearmReleasedOrders()

	// ── AND ASK THE FLEET WHETHER IT AGREES THESE MISSIONS EXIST ──────────
	//
	// Re-registering an order into the tracker is Core saying "I am still
	// commanding this mission". Nothing has ever checked the other half of that
	// sentence. Against a real RDS the check is nearly always redundant: it is a
	// separate durable process and this restart did not touch it. Against the
	// in-process simulator it is the entire story — the restart emptied the
	// fleet, and every mission reloaded here is one Core will drive, append to,
	// and wait on forever, against nobody.
	//
	// One measured window was read as a §R.91 regression on exactly this. The
	// line below is what would have said so in the first thirty seconds.
	//
	// It reports and does nothing else. Not knowing where a robot is has never
	// been grounds for terminating an order and is not grounds now (§R.98,
	// refused 4/4) — the fix for a mission the fleet has lost is upstream, in
	// the backends that now refuse to append to one.
	missing := 0
	if reg, ok := e.fleet.(fleet.MissionRegistry); ok {
		for _, id := range ids {
			if !reg.HasOrder(id) {
				missing++
				e.logFn("engine: !! vendor order %s was reloaded into the tracker but %s does not hold it — Core is commanding a mission that does not exist",
					id, e.fleet.Name())
			}
		}
	}
	if missing > 0 {
		e.logFn("engine: !! loaded %d active vendor orders into tracker and %s knows about %d of them — %d mission(s) are Core's alone",
			len(ids), e.fleet.Name(), len(ids)-missing, missing)
		return
	}
	e.logFn("engine: loaded %d active vendor orders into tracker", len(ids))
}

// newDispatcher builds the dispatcher Start runs. A refusal at intake carries
// the touched nodes' bins rows; the read is the node service's, wired later
// (SetBinsRowsFunc) and asked at call time.
func (e *Engine) newDispatcher(emitter dispatch.Emitter, resolver *dispatch.DefaultResolver) *dispatch.Dispatcher {
	d := dispatch.NewDispatcher(
		e.db,
		e.fleet,
		emitter,
		e.cfg.Messaging.StationID,
		e.cfg.Messaging.DispatchTopic,
		resolver,
	)
	d.SetBinsRowsFunc(e.nodeService.BinsRows)
	// A refusal's sentence writes its time on the plant's clock: the zone Core's
	// screens use (www's plantLocation), resolved the same way.
	d.SetPlantZone(planttime.ResolveZone(e.cfg.Timezone).Loc)
	// The table turn: the fleet's turner, if it has one, and the robot status
	// from the poll robotRefreshLoop already makes (dispatch/table_turn.go).
	if t, ok := e.fleet.(fleet.TableTurner); ok {
		d.SetTableTurner(t, e.polledRobotStatus)
	}
	d.SetTableTurnAlarm(e.tableTurnAlarm)
	// The carrier families that name a binTask key (dispatch/carrier_keys.go).
	// Unread, the set is empty and every block sends the default until the lane
	// floor's next tick reads it.
	if _, err := d.ReloadCarrierKeys(); err != nil {
		e.logFn("engine: carrier keys: %v (every block sends the default key until the next read)", err)
	}
	return d
}

// tableTurnAlarm is what a table that never turned raises beyond its row and
// its audit line: a log line, and the plant's notification email when one is
// configured.
func (e *Engine) tableTurnAlarm(orderID int64, node, detail string) {
	e.logFn("engine: TABLE TURN FAILED at %s (order %d): %s", node, orderID, detail)
	if e.notifier == nil || !e.notifier.Enabled() {
		return
	}
	subject, body := notify.TableTurnFailedSubject(node), notify.TableTurnFailedAlert(orderID, node, detail)
	go func() { _ = e.notifier.Send(subject, body) }()
}

// polledRobotFresh is how old the latest robot poll may be for the table turn
// to act on it: three poll intervals. Older, and a robot's table angle is a
// memory, not a reading.
const polledRobotFresh = 3 * protocol.RobotPollInterval

// polledRobotStatus is the table turn's robot read: the robot as the latest
// poll listed it, and false when that poll is stale or does not list it. It
// reads the poll rather than robotsCache, which keeps a robot RDS stopped
// listing forever.
func (e *Engine) polledRobotStatus(vehicleID string) (fleet.RobotStatus, bool) {
	robots, at := e.latestPolledRobots()
	if at.IsZero() || time.Since(at) > polledRobotFresh {
		return fleet.RobotStatus{}, false
	}
	for _, r := range robots {
		if r.VehicleID == vehicleID {
			return r, true
		}
	}
	return fleet.RobotStatus{}, false
}

// reportSetups names, at boot, the setups saved half-finished or before a rule
// refused them, so the owner can see how the plant is set up:
//
//   - the nodes whose bin-type config says "accepts nothing" — mode `specific`
//     with no types assigned. Every reader in dispatch treats an empty list as
//     unrestricted, so this shape is silently permissive everywhere except the
//     stranded-bin placement gate, which refuses it. One line at boot is how a
//     half-finished config gets finished;
//   - the claim legs where Core picks that name a member of a node group
//     (reportMemberLegs);
//   - the nodes with no name (reportNamelessNodes).
func (e *Engine) reportSetups() {
	if names, err := e.db.NodesAcceptingNothing(); err != nil {
		e.dbg("engine: audit nodes accepting no bin type: %v", err)
	} else if len(names) > 0 {
		e.logFn("engine: %d node(s) are set to accept specific bin types but have none "+
			"assigned, so their config accepts nothing: %s", len(names), strings.Join(names, ", "))
	}
	e.reportMemberLegs()
	e.reportNamelessNodes()
}

// reportNamelessNodes names, at start, every node with no name (docket B2,
// nodes.NameIsBlank): by id, with its parent and the bins standing on it. The
// store refuses to write one now, so a row like that was saved before the
// guard or by hand, and no order can name it. It changes nothing: no rename,
// no delete. Read in Core's start-up log, at the first deploy of the guard, to
// confirm a plant has none. Pinned by TestBootReport_NamesANodeWithNoName.
func (e *Engine) reportNamelessNodes() {
	all, err := e.db.ListNodes()
	if err != nil {
		e.dbg("engine: audit nodes with no name: %v", err)
		return
	}
	var said []string
	for _, n := range all {
		if !nodes.NameIsBlank(n.Name) {
			continue
		}
		parent := "no parent"
		if n.ParentID != nil {
			parent = fmt.Sprintf("parent %d %q", *n.ParentID, n.ParentName)
		}
		standing := "no bins"
		if bs, err := e.db.ListBinsByNode(n.ID); err != nil {
			standing = fmt.Sprintf("bins unread: %v", err)
		} else if len(bs) > 0 {
			labels := make([]string, len(bs))
			for i, b := range bs {
				labels[i] = b.Label
			}
			standing = "bins " + strings.Join(labels, ", ")
		}
		said = append(said, fmt.Sprintf("node %d (%s; %s)", n.ID, parent, standing))
	}
	if len(said) > 0 {
		e.logFn("engine: %d node(s) have no name, so no order can name one; Core changes nothing: %s",
			len(said), strings.Join(said, "; "))
	}
}

// reportMemberLegs logs, at start, every claim leg where Core picks (inbound
// source, outbound, containment and partial destinations) that names a member
// of a node group, and which of those members are loader homes. It refuses
// nothing: what Core does with each is unchanged, except containment, which
// parks (dispatch.CauseContainmentMember).
func (e *Engine) reportMemberLegs() {
	legs, err := e.db.ClaimPickLegsNamingMembers()
	if err != nil {
		e.dbg("engine: audit claim legs naming a group member: %v", err)
		return
	}
	if len(legs) == 0 {
		return
	}
	homes := 0
	for _, l := range legs {
		if l.LoaderHome {
			homes++
		}
	}
	e.logFn("engine: %d claim leg(s) where Core picks name a member of a node group (%d of them a loader home); "+
		"the Edge refuses these now: name the group, or a node in no group", len(legs), homes)
	for _, l := range legs {
		home := ""
		if l.LoaderHome {
			home = " (a loader home)"
		}
		e.logFn("engine:   claim %s / %s at %s: %s = %s, a member of %s%s",
			l.Process, l.Style, l.Node, l.Leg, l.Name, l.Group, home)
	}
}

// startReconciliationLoop starts the periodic reconciliation (logging and
// auto-confirm), first saying once whether the config still sets the retired
// abandon keys.
func (e *Engine) startReconciliationLoop() {
	e.sayRetiredAbandonKeys()
	e.loops.spawn(func() {
		e.reconciliation.Loop(e.stopChan, e.cfg.Staging.SweepInterval, e.cfg.Staging.AutoConfirmDelivered)
	})
}

// sayRetiredAbandonKeys logs, once at start, a config file that still sets the
// retired stuck-order abandon keys: the reconciliation loop no longer runs the
// abandon, and the value does nothing.
func (e *Engine) sayRetiredAbandonKeys() {
	if s := e.cfg.Staging; s.AbandonStuck > 0 || s.AbandonStuckOperatorGated > 0 {
		e.logFn("engine: staging.abandon_stuck (%s) and staging.abandon_stuck_operator_gated (%s) are set and ignored: Core cancels no waiting order on a timer; the Edge sends back robots parked at a line idle past its hour",
			s.AbandonStuck, s.AbandonStuckOperatorGated)
	}
}
