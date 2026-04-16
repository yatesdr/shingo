// engine.go — ShinGo Core engine: struct, lifecycle, and subsystem wiring.
//
// Layout:
//   Config / Engine struct       – dependencies and internal state
//   New / Start / Stop           – lifecycle (Start wires dispatcher, tracker,
//                                  fulfillment scanner, event handlers, background loops)
//   Accessors                    – one-liner getters for subsystems
//   SendToEdge / SendDataToEdge  – outbound envelope helpers
//   Connection health            – checkConnectionStatus, connectionHealthLoop
//   Scene sync                   – SyncScenePoints, SyncFleetNodes, UpdateNodeZones
//   Background loops             – robotRefreshLoop, stagedBinSweepLoop
//   Reconfigure*                 – live-reload for DB, fleet, messaging
//
// To find where a background loop is started, search Start() for "go e.".

package engine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/countgroup"
	"shingocore/dispatch"
	"shingocore/fleet"
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
	Debug      bool
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
	countGroup      *countgroup.Runner                      // nil if feature disabled / no groups configured
	countGroupBuild func(countgroup.Emitter) *countgroup.Runner // stored for ReconfigureCountGroups
	Events          *EventBus
	logFn           LogFunc
	debugLog        func(string, ...any)
	reconciliation  *ReconciliationService
	recovery        *RecoveryService
	fulfillment     *FulfillmentScanner
	binManifest     *service.BinManifestService
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

// ── Lifecycle ───────────────────────────────────────────────────────

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
	e.fulfillment = newFulfillmentScanner(e.db, e.dispatcher, resolver, e.sendToEdge, e.failOrderAndEmit, e.logFn, e.debugLog)

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

// ── Accessors ───────────────────────────────────────────────────────

func (e *Engine) DB() *store.DB                          { return e.db }
func (e *Engine) AppConfig() *config.Config              { return e.cfg }
func (e *Engine) ConfigPath() string                     { return e.configPath }
func (e *Engine) Dispatcher() *dispatch.Dispatcher       { return e.dispatcher }
func (e *Engine) Tracker() fleet.OrderTracker            { return e.tracker }
func (e *Engine) Fleet() fleet.Backend                   { return e.fleet }
func (e *Engine) MsgClient() *messaging.Client           { return e.msgClient }
func (e *Engine) Reconciliation() *ReconciliationService { return e.reconciliation }
func (e *Engine) Recovery() *RecoveryService             { return e.recovery }
func (e *Engine) BinManifest() *service.BinManifestService { return e.binManifest }

// SetCountGroupRunner registers a configured Runner built by the
// composition root. The caller passes the Runner directly — transitions
// land on the engine's EventBus via the internal emitter adapter.
// Engine.Start() will call .Start() on it; Engine.Stop() will call .Stop().
// Pass nil (or just don't call) to disable the feature.
//
// Takes a factory function that receives the EventBus-backed emitter so
// the caller can build the Runner without the engine exposing emitter
// construction as part of its public API.
func (e *Engine) SetCountGroupRunner(build func(countgroup.Emitter) *countgroup.Runner) {
	if build == nil {
		return
	}
	e.countGroupBuild = build
	e.countGroup = build(&countGroupEventEmitter{bus: e.Events})
}

// ── Outbound messaging ──────────────────────────────────────────────

// SendToEdge is an exported wrapper around sendToEdge, allowing HTTP handlers
// and other external callers to enqueue messages for edge stations via outbox.
func (e *Engine) SendToEdge(msgType string, stationID string, payload any) error {
	return e.sendToEdge(msgType, stationID, payload)
}

// SendDataToEdge builds a data-channel envelope and enqueues it via outbox.
// Used by HTTP handlers to push data notifications (e.g., node structure changes).
func (e *Engine) SendDataToEdge(subject string, stationID string, payload any) error {
	coreAddr := protocol.Address{Role: protocol.RoleCore, Station: e.cfg.Messaging.StationID}
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	env, err := protocol.NewDataEnvelope(subject, coreAddr, edgeAddr, payload)
	if err != nil {
		return fmt.Errorf("build data %s: %w", subject, err)
	}
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode data %s: %w", subject, err)
	}
	msgType := "data." + subject
	if err := e.db.EnqueueOutbox(e.cfg.Messaging.DispatchTopic, data, msgType, stationID); err != nil {
		e.logFn("engine: outbox enqueue data %s to %s failed: %v", subject, stationID, err)
		return fmt.Errorf("enqueue data %s: %w", subject, err)
	}
	return nil
}

// RunFulfillmentScan runs one pass of the fulfillment scanner and returns the
// number of orders processed. For testing.
func (e *Engine) RunFulfillmentScan() int {
	if e.fulfillment == nil {
		return 0
	}
	return e.fulfillment.RunOnce()
}

// ── Connection health ───────────────────────────────────────────────

func (e *Engine) checkConnectionStatus() {
	// Fleet
	if err := e.fleet.Ping(); err == nil {
		if e.fleetConnected.CompareAndSwap(false, true) {
			e.Events.Emit(Event{Type: EventFleetConnected, Payload: ConnectionEvent{Detail: e.fleet.Name() + " connected"}})
			go func() {
				total, created, deleted, err := e.SceneSync()
				if err != nil {
					e.logFn("engine: auto scene sync: %v", err)
					return
				}
				e.logFn("engine: auto scene sync: %d points, created %d, deleted %d nodes", total, created, deleted)
			}()
		}
	} else {
		if e.fleetConnected.CompareAndSwap(true, false) {
			e.Events.Emit(Event{Type: EventFleetDisconnected, Payload: ConnectionEvent{Detail: err.Error()}})
		}
	}

	// Messaging
	if e.msgClient != nil {
		if e.msgClient.IsConnected() {
			if e.msgConnected.CompareAndSwap(false, true) {
				e.Events.Emit(Event{Type: EventMessagingConnected, Payload: ConnectionEvent{Detail: "messaging connected"}})
			}
		} else {
			if e.msgConnected.CompareAndSwap(true, false) {
				e.Events.Emit(Event{Type: EventMessagingDisconnected, Payload: ConnectionEvent{Detail: "messaging disconnected"}})
			}
		}
	}

	// Database
	if err := e.db.Ping(); err == nil {
		if e.dbConnected.CompareAndSwap(false, true) {
			e.Events.Emit(Event{Type: EventDBConnected, Payload: ConnectionEvent{Detail: "database connected"}})
		}
	} else {
		if e.dbConnected.CompareAndSwap(true, false) {
			e.Events.Emit(Event{Type: EventDBDisconnected, Payload: ConnectionEvent{Detail: err.Error()}})
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

// ── Live reconfiguration ────────────────────────────────────────────

// ReconfigureDatabase reconnects the database with current config.
func (e *Engine) ReconfigureDatabase() {
	if err := e.db.Reconnect(&e.cfg.Database); err != nil {
		e.logFn("engine: database reconfigure error: %v", err)
	} else {
		e.logFn("engine: database reconfigured")
	}
	e.checkConnectionStatus()
}

// ReconfigureFleet applies fleet config changes live.
func (e *Engine) ReconfigureFleet() {
	e.fleet.Reconfigure(fleet.ReconfigureParams{
		BaseURL: e.cfg.RDS.BaseURL,
		Timeout: e.cfg.RDS.Timeout,
	})
	e.logFn("engine: fleet reconfigured (%s)", e.fleet.Name())
	e.checkConnectionStatus()
}

// ReconfigureCountGroups stops the current count-group runner and starts
// a new one with the latest config. Safe to call when the builder is nil
// (feature was never enabled) — it logs and returns.
func (e *Engine) ReconfigureCountGroups() {
	if e.countGroupBuild == nil {
		e.logFn("engine: count-group reconfigure skipped (no builder registered)")
		return
	}

	// Stop the old runner gracefully.
	if e.countGroup != nil {
		e.countGroup.Stop()
		e.logFn("engine: count-group runner stopped for reconfiguration")
	}

	// Build and start a fresh runner — the builder's closure reads
	// cfg.CountGroups at call time, so it picks up the new group list.
	e.countGroup = e.countGroupBuild(&countGroupEventEmitter{bus: e.Events})
	e.countGroup.Start()
	e.logFn("engine: count-group runner reconfigured (%d groups)", len(e.cfg.CountGroups.Groups))
}

// ReconfigureMessaging reconnects messaging with current config.
func (e *Engine) ReconfigureMessaging() {
	if err := e.msgClient.Reconfigure(&e.cfg.Messaging); err != nil {
		e.logFn("engine: messaging reconfigure error: %v", err)
	} else {
		e.logFn("engine: messaging reconfigured")
	}
	e.checkConnectionStatus()
}

// ── Scene sync (fleet → DB → nodes) ─────────────────────────────────

// SyncScenePoints persists fleet scene areas to the database.
// Returns the total number of points synced and a map of bin location instanceName → areaName.
func (e *Engine) SyncScenePoints(areas []fleet.SceneArea) (int, map[string]string) {
	locationSet := make(map[string]string)
	total := 0
	for _, area := range areas {
		if err := e.db.DeleteScenePointsByArea(area.Name); err != nil {
			e.logFn("engine: sync scene: delete points for area %s: %v", area.Name, err)
		}
		for _, ap := range area.AdvancedPoints {
			sp := &store.ScenePoint{
				AreaName:       area.Name,
				InstanceName:   ap.InstanceName,
				ClassName:      ap.ClassName,
				Label:          ap.Label,
				PosX:           ap.PosX,
				PosY:           ap.PosY,
				PosZ:           ap.PosZ,
				Dir:            ap.Dir,
				PropertiesJSON: ap.PropertiesJSON,
			}
			if err := e.db.UpsertScenePoint(sp); err != nil {
				e.logFn("engine: sync scene: upsert point %s: %v", ap.InstanceName, err)
			}
			total++
		}
		for _, bin := range area.BinLocations {
			locationSet[bin.InstanceName] = area.Name
			sp := &store.ScenePoint{
				AreaName:       area.Name,
				InstanceName:   bin.InstanceName,
				ClassName:      bin.ClassName,
				Label:          bin.Label,
				PointName:      bin.PointName,
				GroupName:      bin.GroupName,
				PosX:           bin.PosX,
				PosY:           bin.PosY,
				PosZ:           bin.PosZ,
				PropertiesJSON: bin.PropertiesJSON,
			}
			if err := e.db.UpsertScenePoint(sp); err != nil {
				e.logFn("engine: sync scene: upsert point %s: %v", bin.InstanceName, err)
			}
			total++
		}
	}
	return total, locationSet
}

// SyncFleetNodes creates nodes for new scene locations and removes nodes no longer in the scene.
// Returns the number of nodes created and deleted.
func (e *Engine) SyncFleetNodes(locationSet map[string]string) (created, deleted int) {
	// Look up default storage node type ID
	var storageTypeID *int64
	if nt, err := e.db.GetNodeTypeByCode("STAG"); err == nil {
		storageTypeID = &nt.ID
	}

	// Create nodes for locations not yet in DB (matched by name).
	for instanceName, areaName := range locationSet {
		if existing, err := e.db.GetNodeByName(instanceName); err == nil {
			// Node exists — update zone if needed
			if existing.Zone != areaName && areaName != "" {
				existing.Zone = areaName
				if err := e.db.UpdateNode(existing); err != nil {
					e.logFn("engine: sync fleet nodes: update node %s zone: %v", instanceName, err)
				}
			}
			continue
		}
		node := &store.Node{
			Name:       instanceName,
			NodeTypeID: storageTypeID,
			Zone:       areaName,
			Enabled:    true,
		}
		if err := e.db.CreateNode(node); err != nil {
			e.logFn("engine: sync fleet nodes: create node %q: %v", instanceName, err)
			continue
		}
		e.Events.Emit(Event{Type: EventNodeUpdated, Payload: NodeUpdatedEvent{
			NodeID: node.ID, NodeName: node.Name, Action: "created",
		}})
		created++
	}

	// Delete physical nodes not present in current scene.
	// Skip synthetic nodes (node groups, lanes), nodes
	// without a name, and child nodes (part of a hierarchy)
	// — these are managed by shingo, not the fleet.
	nodes, err := e.db.ListNodes()
	if err != nil {
		e.logFn("engine: sync fleet nodes: list nodes: %v", err)
	}
	for _, n := range nodes {
		if n.IsSynthetic || n.Name == "" || n.ParentID != nil {
			continue
		}
		if _, inScene := locationSet[n.Name]; !inScene {
			if err := e.db.DeleteNode(n.ID); err != nil {
				e.logFn("engine: sync fleet nodes: delete node %s: %v", n.Name, err)
			}
			e.Events.Emit(Event{Type: EventNodeUpdated, Payload: NodeUpdatedEvent{
				NodeID: n.ID, NodeName: n.Name, Action: "deleted",
			}})
			deleted++
		}
	}

	// Update zones on remaining nodes.
	e.UpdateNodeZones(locationSet, true)
	return
}

// UpdateNodeZones updates node zones from a location→area map.
// If overwrite is true, updates zone whenever it differs; if false, only fills empty zones.
func (e *Engine) UpdateNodeZones(locationSet map[string]string, overwrite bool) {
	nodes, err := e.db.ListNodes()
	if err != nil {
		e.logFn("engine: update node zones: list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		zone, ok := locationSet[n.Name]
		if !ok {
			continue
		}
		if !overwrite && n.Zone != "" {
			continue
		}
		if n.Zone == zone {
			continue
		}
		n.Zone = zone
		if err := e.db.UpdateNode(n); err != nil {
			e.logFn("engine: update node %s zone: %v", n.Name, err)
		}
		e.Events.Emit(Event{Type: EventNodeUpdated, Payload: NodeUpdatedEvent{
			NodeID: n.ID, NodeName: n.Name, Action: "updated",
		}})
	}
}

// SceneSync loads scene data from the fleet backend and syncs nodes.
// It is guarded by an atomic bool to prevent concurrent runs.
func (e *Engine) SceneSync() (int, int, int, error) {
	if !e.sceneSyncing.CompareAndSwap(false, true) {
		return 0, 0, 0, fmt.Errorf("scene sync already in progress")
	}
	defer e.sceneSyncing.Store(false)

	syncer, ok := e.fleet.(fleet.SceneSyncer)
	if !ok {
		return 0, 0, 0, fmt.Errorf("fleet backend does not support scene sync")
	}
	areas, err := syncer.GetSceneAreas()
	if err != nil {
		return 0, 0, 0, err
	}
	total, locSet := e.SyncScenePoints(areas)
	created, deleted := e.SyncFleetNodes(locSet)
	return total, created, deleted, nil
}

// ── Background loops ────────────────────────────────────────────────

// robotRefreshLoop polls robot status every 2 seconds and emits EventRobotsUpdated
// only when the robot state has actually changed.
func (e *Engine) robotRefreshLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var prevHash [sha256.Size]byte
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			if !e.fleetConnected.Load() {
				continue
			}
			rl, ok := e.fleet.(fleet.RobotLister)
			if !ok {
				continue
			}
			robots, err := rl.GetRobotsStatus()
			if err != nil {
				e.dbg("engine: robot refresh: %v", err)
				continue
			}
			// Update robot position cache (used for telemetry snapshots)
			e.robotsMu.Lock()
			for _, r := range robots {
				e.robotsCache[r.VehicleID] = r
			}
			e.robotsMu.Unlock()

			data, _ := json.Marshal(robots)
			hash := sha256.Sum256(data)
			if hash == prevHash {
				continue
			}
			prevHash = hash
			e.Events.Emit(Event{
				Type:    EventRobotsUpdated,
				Payload: RobotsUpdatedEvent{Robots: robots},
			})
		}
	}
}

// stagedBinSweepLoop periodically releases staged bins whose expiry has passed.
func (e *Engine) stagedBinSweepLoop() {
	interval := e.cfg.Staging.SweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			count, err := e.db.ReleaseExpiredStagedBins()
			if err != nil {
				e.logFn("engine: staged bin sweep error: %v", err)
			} else if count > 0 {
				e.logFn("engine: released %d expired staged bins", count)
			}
			orphaned, err := e.db.ReleaseOrphanedClaims()
			if err != nil {
				e.logFn("engine: orphan claim sweep error: %v", err)
			} else if orphaned > 0 {
				e.logFn("engine: released %d orphaned bin claims from terminal orders", orphaned)
			}
		}
	}
}

// ── Adapters ────────────────────────────────────────────────────────

// orderResolver implements fleet.OrderIDResolver.
type orderResolver struct {
	db *store.DB
}

func (r *orderResolver) ResolveVendorOrderID(vendorOrderID string) (int64, error) {
	order, err := r.db.GetOrderByVendorID(vendorOrderID)
	if err != nil {
		return 0, err
	}
	return order.ID, nil
}
