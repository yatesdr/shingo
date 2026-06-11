package messaging

import (
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"shingo/protocol"
)

// ActiveOrderCountFunc returns the number of active (non-terminal) orders.
type ActiveOrderCountFunc func() int

// CellCatalogFunc returns the edge's cell catalog (PLC-grouped reporting points)
// to attach to the registration payload (Q-034). Nil or a nil return means "no
// catalog" — an old/absent catalog is not an error.
type CellCatalogFunc func() []protocol.CellCatalogEntry

// Heartbeater sends edge.register on startup and edge.heartbeat periodically.
type Heartbeater struct {
	sender       *DataSender
	stationID    string
	version      string
	lineIDs      []string
	interval     time.Duration
	startTime    time.Time
	orderCountFn ActiveOrderCountFunc

	stopOnce sync.Once
	stopCh   chan struct{}

	// CatalogFn, when set, supplies the Q-034 cell catalog included on every
	// register (startup + reconnect). Set post-construction, like DebugLog.
	CatalogFn CellCatalogFunc

	DebugLog DebugLogFunc
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

// Stop halts the heartbeat loop. Deliberately NOT called on any shutdown path:
// the heartbeater is a process-lifetime component (main.go starts it and does
// not defer Stop — the Kafka client close tears the goroutine down at exit).
// Investigated as a possible missing-shutdown-call bug and resolved as
// intentional, not a defect. Kept as a real method for a future graceful-
// shutdown path that needs to stop heartbeats before process exit.
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
	var catalog []protocol.CellCatalogEntry
	if h.CatalogFn != nil {
		catalog = h.CatalogFn()
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectEdgeRegister,
		protocol.Address{Role: protocol.RoleEdge, Station: h.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeRegister{
			StationID: h.stationID,
			Hostname:  hostname,
			Version:   h.version,
			LineIDs:   h.lineIDs,
			Catalog:   catalog,
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

func (h *Heartbeater) loop() {
	// Recover-and-restart-with-5s-backoff. The earlier
	// recover-and-exit silently killed the goroutine on panic — the
	// process kept running but heartbeats permanently stopped,
	// Core's deadman tripped ~30s later, and the edge got marked
	// stale in a loop. Loud restart is strictly more diagnostic
	// than silent exit.
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("PANIC heartbeater-loop: %v\n%s", r, debug.Stack())
					log.Printf("WARNING heartbeater restarting after 5s — if this repeats, Core will mark this edge stale")
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
					tick++
					// Re-request node list and payload catalog every
					// other tick (base interval is 60s → ~2 min poll).
					// coreNodes is display-only per
					// operator_guards.go:15-22 — no dispatch / routing
					// decision reads from it — so this is purely a
					// freshness improvement.
					if tick%2 == 0 {
						h.sendNodeListRequest()
						h.sendCatalogRequest()
					}
				}
			}
		}()
		// If the inner loop returned cleanly via stopCh, exit.
		select {
		case <-h.stopCh:
			return
		case <-time.After(5 * time.Second):
			// Restart the loop after the backoff. If the panic is
			// deterministic, the warning log fires every 5s — loud
			// signal. If transient, the loop self-heals before Core's
			// deadman trips.
		}
	}
}
