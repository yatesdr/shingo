package messaging

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store"
)

// CoreHandler handles inbound protocol messages on the orders topic.
// It processes registration and heartbeat messages directly, and
// delegates order messages to the dispatcher.
type CoreHandler struct {
	protocol.NoOpHandler

	db         *store.DB
	client     *Client
	stationID  string
	dispatchTopic string
	dispatcher *dispatch.Dispatcher

	// Background goroutine for stale edge detection
	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewCoreHandler creates a handler for inbound edge messages.
func NewCoreHandler(db *store.DB, client *Client, stationID, dispatchTopic string, dispatcher *dispatch.Dispatcher) *CoreHandler {
	return &CoreHandler{
		db:            db,
		client:        client,
		stationID:     stationID,
		dispatchTopic: dispatchTopic,
		dispatcher:    dispatcher,
		stopCh:        make(chan struct{}),
	}
}

// Start begins the stale-edge detection goroutine.
func (h *CoreHandler) Start() {
	go h.staleEdgeLoop()
}

// Stop halts the stale-edge detection goroutine.
func (h *CoreHandler) Stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
}

func (h *CoreHandler) HandleData(env *protocol.Envelope, p *protocol.Data) {
	switch p.Subject {
	case protocol.SubjectEdgeRegister:
		var reg protocol.EdgeRegister
		if err := json.Unmarshal(p.Body, &reg); err != nil {
			log.Printf("core_handler: decode edge register body: %v", err)
			return
		}
		h.handleEdgeRegister(env, &reg)
	case protocol.SubjectEdgeHeartbeat:
		var hb protocol.EdgeHeartbeat
		if err := json.Unmarshal(p.Body, &hb); err != nil {
			log.Printf("core_handler: decode edge heartbeat body: %v", err)
			return
		}
		h.handleEdgeHeartbeat(env, &hb)
	case protocol.SubjectNodeListRequest:
		h.handleNodeListRequest(env)
	case protocol.SubjectProductionReport:
		var rpt protocol.ProductionReport
		if err := json.Unmarshal(p.Body, &rpt); err != nil {
			log.Printf("core_handler: decode production report body: %v", err)
			return
		}
		h.handleProductionReport(env, &rpt)
	default:
		log.Printf("core_handler: unhandled data subject: %s", p.Subject)
	}
}

func (h *CoreHandler) handleEdgeRegister(env *protocol.Envelope, p *protocol.EdgeRegister) {
	log.Printf("core_handler: edge registered: %s (hostname=%s, version=%s, lines=%v)",
		p.StationID, p.Hostname, p.Version, p.LineIDs)

	if err := h.db.RegisterEdge(p.StationID, p.Hostname, p.Version, p.LineIDs); err != nil {
		log.Printf("core_handler: register edge %s: %v", p.StationID, err)
		return
	}

	reply, err := protocol.NewDataReply(
		protocol.SubjectEdgeRegistered,
		protocol.Address{Role: protocol.RoleCore, Station: h.stationID},
		protocol.Address{Role: protocol.RoleEdge, Station: p.StationID},
		env.ID,
		&protocol.EdgeRegistered{StationID: p.StationID, Message: "registered"},
	)
	if err != nil {
		log.Printf("core_handler: build registered reply: %v", err)
		return
	}

	if err := h.client.PublishEnvelope(h.dispatchTopic, reply); err != nil {
		log.Printf("core_handler: publish registered reply: %v", err)
	}
}

func (h *CoreHandler) handleEdgeHeartbeat(env *protocol.Envelope, p *protocol.EdgeHeartbeat) {
	if err := h.db.UpdateHeartbeat(p.StationID); err != nil {
		log.Printf("core_handler: update heartbeat for %s: %v", p.StationID, err)
		return
	}

	reply, err := protocol.NewDataReply(
		protocol.SubjectEdgeHeartbeatAck,
		protocol.Address{Role: protocol.RoleCore, Station: h.stationID},
		protocol.Address{Role: protocol.RoleEdge, Station: p.StationID},
		env.ID,
		&protocol.EdgeHeartbeatAck{StationID: p.StationID, ServerTS: time.Now().UTC()},
	)
	if err != nil {
		log.Printf("core_handler: build heartbeat ack: %v", err)
		return
	}

	if err := h.client.PublishEnvelope(h.dispatchTopic, reply); err != nil {
		log.Printf("core_handler: publish heartbeat ack: %v", err)
	}
}

func (h *CoreHandler) handleNodeListRequest(env *protocol.Envelope) {
	nodes, err := h.db.ListNodes()
	if err != nil {
		log.Printf("core_handler: list nodes for %s: %v", env.Src.Station, err)
		return
	}
	infos := make([]protocol.NodeInfo, len(nodes))
	for i, n := range nodes {
		infos[i] = protocol.NodeInfo{Name: n.Name, NodeType: n.NodeType}
	}
	reply, err := protocol.NewDataReply(
		protocol.SubjectNodeListResponse,
		protocol.Address{Role: protocol.RoleCore, Station: h.stationID},
		protocol.Address{Role: protocol.RoleEdge, Station: env.Src.Station},
		env.ID,
		&protocol.NodeListResponse{Nodes: infos},
	)
	if err != nil {
		log.Printf("core_handler: build node list reply: %v", err)
		return
	}
	if err := h.client.PublishEnvelope(h.dispatchTopic, reply); err != nil {
		log.Printf("core_handler: publish node list reply: %v", err)
	} else {
		log.Printf("core_handler: sent node list (%d nodes) to %s", len(infos), env.Src.Station)
	}
}

// Order message handlers delegate to the dispatcher.

func (h *CoreHandler) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	log.Printf("core_handler: order request from %s: uuid=%s type=%s", env.Src.Station, p.OrderUUID, p.OrderType)
	h.dispatcher.HandleOrderRequest(env, p)
}

func (h *CoreHandler) HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel) {
	log.Printf("core_handler: order cancel from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderCancel(env, p)
}

func (h *CoreHandler) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	log.Printf("core_handler: delivery receipt from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderReceipt(env, p)
}

func (h *CoreHandler) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	log.Printf("core_handler: redirect from %s: uuid=%s -> %s", env.Src.Station, p.OrderUUID, p.NewDeliveryNode)
	h.dispatcher.HandleOrderRedirect(env, p)
}

func (h *CoreHandler) HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill) {
	log.Printf("core_handler: storage waybill from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderStorageWaybill(env, p)
}

func (h *CoreHandler) handleProductionReport(env *protocol.Envelope, rpt *protocol.ProductionReport) {
	log.Printf("core_handler: production report from %s: %d entries", rpt.StationID, len(rpt.Reports))
	for _, entry := range rpt.Reports {
		if entry.CatID == "" || entry.Count <= 0 {
			continue
		}
		if err := h.db.IncrementProduced(entry.CatID, entry.Count); err != nil {
			log.Printf("core_handler: increment produced %s: %v", entry.CatID, err)
		}
		if err := h.db.LogProduction(entry.CatID, rpt.StationID, entry.Count); err != nil {
			log.Printf("core_handler: log production %s: %v", entry.CatID, err)
		}
	}
}

func (h *CoreHandler) staleEdgeLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			n, err := h.db.MarkStaleEdges(180 * time.Second)
			if err != nil {
				log.Printf("core_handler: mark stale edges: %v", err)
			} else if n > 0 {
				log.Printf("core_handler: marked %d edge(s) stale", n)
			}
		}
	}
}
