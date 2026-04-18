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

	stopOnce sync.Once
	stopCh   chan struct{}

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

// RequestNodeState sends a node state request to core for the given node names.
// TODO(dead-code): no callers as of 2026-04-17; verify before the next refactor.
func (h *Heartbeater) RequestNodeState(nodes []string) {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectNodeStateRequest,
		protocol.Address{Role: protocol.RoleEdge, Station: h.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.NodeStateRequest{Nodes: nodes},
	)
	if err != nil {
		log.Printf("heartbeater: build node state request: %v", err)
		return
	}
	h.sender.DebugLog = h.DebugLog
	if err := h.sender.PublishEnvelope(env, "node state request"); err != nil {
		log.Printf("heartbeater: send node state request failed: %v", err)
	} else {
		log.Printf("heartbeater: sent node.state_request (%d nodes)", len(nodes))
	}
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
			tick++
			if tick%5 == 0 { // re-request node list and payload catalog every ~5 min
				h.sendNodeListRequest()
				h.sendCatalogRequest()
			}
		}
	}
}
