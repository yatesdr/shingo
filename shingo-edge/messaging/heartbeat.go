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
	client    *Client
	stationID string
	version   string
	lineIDs   []string
	topic     string // orders topic to publish on
	interval  time.Duration
	startTime time.Time
	orderCountFn ActiveOrderCountFunc

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewHeartbeater creates a heartbeater for the given edge identity.
func NewHeartbeater(client *Client, stationID, version string, lineIDs []string, ordersTopic string, orderCountFn ActiveOrderCountFunc) *Heartbeater {
	return &Heartbeater{
		client:       client,
		stationID:    stationID,
		version:      version,
		lineIDs:      lineIDs,
		topic:        ordersTopic,
		interval:     60 * time.Second,
		orderCountFn: orderCountFn,
		stopCh:       make(chan struct{}),
	}
}

// Start sends an initial registration, requests the core node list, and begins the heartbeat loop.
func (h *Heartbeater) Start() {
	h.startTime = time.Now()
	h.sendRegister()
	h.sendNodeListRequest()
	go h.loop()
}

// Stop halts the heartbeat loop.
func (h *Heartbeater) Stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
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
	if err := h.publishWithRetry(env, "register"); err != nil {
		log.Printf("heartbeater: send register failed after retries: %v", err)
	} else {
		log.Printf("heartbeater: sent edge.register (station=%s)", h.stationID)
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
	if err := h.publishWithRetry(env, "node list request"); err != nil {
		log.Printf("heartbeater: send node list request failed after retries: %v", err)
	} else {
		log.Printf("heartbeater: sent node.list_request (station=%s)", h.stationID)
	}
}

// publishWithRetry attempts to publish an envelope with exponential backoff (3 attempts, 2s/4s/8s).
func (h *Heartbeater) publishWithRetry(env *protocol.Envelope, label string) error {
	var err error
	backoff := 2 * time.Second
	for attempt := 0; attempt < 3; attempt++ {
		if err = h.client.PublishEnvelope(h.topic, env); err == nil {
			return nil
		}
		log.Printf("heartbeater: %s attempt %d failed: %v (retrying in %s)", label, attempt+1, err, backoff)
		select {
		case <-h.stopCh:
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}

// RequestNodeSync sends a node list request to core on demand.
func (h *Heartbeater) RequestNodeSync() {
	h.sendNodeListRequest()
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
	if err := h.client.PublishEnvelope(h.topic, env); err != nil {
		log.Printf("heartbeater: send heartbeat: %v", err)
	}
}

func (h *Heartbeater) loop() {
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
			if tick%5 == 0 { // re-request node list every ~5 min
				h.sendNodeListRequest()
			}
		}
	}
}
