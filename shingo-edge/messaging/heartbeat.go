package messaging

import (
	"log"
	"os"
	"sync"
	"time"

	"shingo/protocol"
)

// Heartbeater sends edge.register on startup and edge.heartbeat periodically.
type Heartbeater struct {
	client    *Client
	stationID string
	version   string
	lineIDs   []string
	topic     string // orders topic to publish on
	interval  time.Duration
	startTime time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewHeartbeater creates a heartbeater for the given edge identity.
func NewHeartbeater(client *Client, stationID, version string, lineIDs []string, ordersTopic string) *Heartbeater {
	return &Heartbeater{
		client:    client,
		stationID: stationID,
		version:   version,
		lineIDs:   lineIDs,
		topic:     ordersTopic,
		interval:  60 * time.Second,
		stopCh:    make(chan struct{}),
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
	if err := h.client.PublishEnvelope(h.topic, env); err != nil {
		log.Printf("heartbeater: send register: %v", err)
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
	if err := h.client.PublishEnvelope(h.topic, env); err != nil {
		log.Printf("heartbeater: send node list request: %v", err)
	} else {
		log.Printf("heartbeater: sent node.list_request (station=%s)", h.stationID)
	}
}

// RequestNodeSync sends a node list request to core on demand.
func (h *Heartbeater) RequestNodeSync() {
	h.sendNodeListRequest()
}

func (h *Heartbeater) sendHeartbeat() {
	uptime := int64(time.Since(h.startTime).Seconds())
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectEdgeHeartbeat,
		protocol.Address{Role: protocol.RoleEdge, Station: h.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeHeartbeat{
			StationID: h.stationID,
			Uptime:    uptime,
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
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.sendHeartbeat()
		}
	}
}
