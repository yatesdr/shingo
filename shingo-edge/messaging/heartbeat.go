package messaging

import (
	"log"
	"os"
	"sync"
	"time"

	"shingo/protocol"
)

// ActiveOrderCountFunc returns the number of active (non-terminal) orders.
type ActiveOrderCountFunc func() int

// Heartbeater sends edge.register on startup and edge.heartbeat periodically.
type Heartbeater struct {
	sender       *DataSender
	stationID    string
	version      string
	lineIDs      []string
	interval     time.Duration
	startTime    time.Time
	orderCountFn ActiveOrderCountFunc

	// reconcileFn fires once per heartbeat tick after the heartbeat
	// envelope ships. Wired from the composition root to engine.Reconcile.
	// Phase 2's UOP reconciler piggybacks on the heartbeat cadence per
	// the bin-as-truth plan: no separate goroutine, no separate clock.
	// Nil-safe — tests that don't care about reconciliation leave it unset.
	reconcileFn func()

	stopOnce sync.Once
	stopCh   chan struct{}

	DebugLog DebugLogFunc
}

// SetReconcileFn installs the per-heartbeat reconciliation callback.
// Called by the composition root after the engine is ready. Safe to
// call before Start. Calling with nil disables the trigger (defensive
// — production wires a real callback).
func (h *Heartbeater) SetReconcileFn(fn func()) {
	h.reconcileFn = fn
}

// NewHeartbeater creates a heartbeater for the given edge identity.
func NewHeartbeater(client *Client, stationID, version string, lineIDs []string, ordersTopic string, orderCountFn ActiveOrderCountFunc) *Heartbeater {
	stopCh := make(chan struct{})
	return &Heartbeater{
		sender:       NewDataSender(client, ordersTopic, stopCh),
		stationID:    stationID,
		version:      version,
		lineIDs:      lineIDs,
		interval:     60 * time.Second,
		orderCountFn: orderCountFn,
		stopCh:       stopCh,
	}
}

// Start sends an initial registration, requests the core node list, and begins the heartbeat loop.
func (h *Heartbeater) Start() {
	h.startTime = time.Now()
	h.sendRegister()
	h.sendNodeListRequest()
	h.sendCatalogRequest()
	go h.loop()
}

// Stop halts the heartbeat loop.
func (h *Heartbeater) Stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
}

// SendRegister sends an edge.register message to core. Called on startup
// and when core requests re-registration.
func (h *Heartbeater) SendRegister() {
	h.sendRegister()
}

func (h *Heartbeater) sendRegister() {
	hostname, _ := os.Hostname()
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectEdgeRegister,
		protocol.Address{Role: protocol.RoleEdge, Station: h.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeRegister{
			StationID: h.stationID,
			Hostname:  hostname,
			Version:   h.version,
			LineIDs:   h.lineIDs,
		},
	)
	if err != nil {
		log.Printf("heartbeater: build register: %v", err)
		return
	}
	h.sender.DebugLog = h.DebugLog
	if err := h.sender.PublishEnvelope(env, "register"); err != nil {
		log.Printf("heartbeater: send register failed after retries: %v", err)
	} else {
		log.Printf("heartbeater: sent edge.register (station=%s)", h.stationID)
		h.DebugLog.Log("register sent station=%s", h.stationID)
	}
}

func (h *Heartbeater) sendNodeListRequest() {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectNodeListRequest,
		protocol.Address{Role: protocol.RoleEdge, Station: h.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.NodeListRequest{},
	)
	if err != nil {
		log.Printf("heartbeater: build node list request: %v", err)
		return
	}
	h.sender.DebugLog = h.DebugLog
	if err := h.sender.PublishEnvelope(env, "node list request"); err != nil {
		log.Printf("heartbeater: send node list request failed after retries: %v", err)
	} else {
		log.Printf("heartbeater: sent node.list_request (station=%s)", h.stationID)
		h.DebugLog.Log("node_list_request sent station=%s", h.stationID)
	}
}

// RequestNodeSync sends a node list request to core on demand.
func (h *Heartbeater) RequestNodeSync() {
	h.sendNodeListRequest()
}

// RequestCatalogSync sends a payload catalog request to core on demand.
func (h *Heartbeater) RequestCatalogSync() {
	h.sendCatalogRequest()
}

func (h *Heartbeater) sendCatalogRequest() {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectCatalogPayloadsRequest,
		protocol.Address{Role: protocol.RoleEdge, Station: h.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.CatalogPayloadsRequest{},
	)
	if err != nil {
		log.Printf("heartbeater: build catalog request: %v", err)
		return
	}
	h.sender.DebugLog = h.DebugLog
	if err := h.sender.PublishEnvelope(env, "catalog request"); err != nil {
		log.Printf("heartbeater: send catalog request failed after retries: %v", err)
	} else {
		log.Printf("heartbeater: sent catalog.payloads_request (station=%s)", h.stationID)
		h.DebugLog.Log("catalog_request sent station=%s", h.stationID)
	}
}

func (h *Heartbeater) sendHeartbeat() {
	uptime := int64(time.Since(h.startTime).Seconds())
	var activeOrders int
	if h.orderCountFn != nil {
		activeOrders = h.orderCountFn()
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectEdgeHeartbeat,
		protocol.Address{Role: protocol.RoleEdge, Station: h.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeHeartbeat{
			StationID: h.stationID,
			Uptime:    uptime,
			Orders:    activeOrders,
		},
	)
	if err != nil {
		log.Printf("heartbeater: build heartbeat: %v", err)
		return
	}
	h.sender.DebugLog = h.DebugLog
	if err := h.sender.PublishEnvelope(env, "heartbeat"); err != nil {
		log.Printf("heartbeater: send heartbeat failed after retries: %v", err)
	} else {
		h.DebugLog.Log("heartbeat sent uptime=%ds orders=%d", uptime, activeOrders)
	}
}

// fireReconcile invokes the per-heartbeat reconcile callback if set.
// Item 1: the UOP reconciler piggybacks on the heartbeat cadence per
// the bin-as-truth plan — no separate goroutine, no separate clock.
// Wrapped in a recover so a reconciler panic doesn't take down the
// heartbeat loop. Exported only on the package boundary; the loop
// inside calls it after every successful sendHeartbeat.
func (h *Heartbeater) fireReconcile() {
	if h.reconcileFn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("heartbeater: panic in reconcile callback: %v", r)
		}
	}()
	h.reconcileFn()
}

func (h *Heartbeater) loop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("heartbeater: panic in loop: %v", r)
		}
	}()
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.sendHeartbeat()
			h.fireReconcile()
			tick++
			if tick%5 == 0 { // re-request node list and payload catalog every ~5 min
				h.sendNodeListRequest()
				h.sendCatalogRequest()
			}
		}
	}
}
