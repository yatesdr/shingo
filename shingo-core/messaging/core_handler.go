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
	DebugLog   func(string, ...any)

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

func (h *CoreHandler) dbg(format string, args ...any) {
	if fn := h.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// coreAddr returns the core-side protocol address.
func (h *CoreHandler) coreAddr() protocol.Address {
	return protocol.Address{Role: protocol.RoleCore, Station: h.stationID}
}

// replyData builds and publishes a data reply envelope to the requesting edge station.
func (h *CoreHandler) replyData(env *protocol.Envelope, subject string, payload any) {
	dst := protocol.Address{Role: protocol.RoleEdge, Station: env.Src.Station}
	reply, err := protocol.NewDataReply(subject, h.coreAddr(), dst, env.ID, payload)
	if err != nil {
		log.Printf("core_handler: build reply %s: %v", subject, err)
		return
	}
	if err := h.client.PublishEnvelope(h.dispatchTopic, reply); err != nil {
		log.Printf("core_handler: publish reply %s: %v", subject, err)
	}
}

// sendData builds and publishes a data envelope (not a reply) to a specific station.
func (h *CoreHandler) sendData(subject, stationID string, payload any) {
	dst := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	env, err := protocol.NewDataEnvelope(subject, h.coreAddr(), dst, payload)
	if err != nil {
		log.Printf("core_handler: build %s for %s: %v", subject, stationID, err)
		return
	}
	if err := h.client.PublishEnvelope(h.dispatchTopic, env); err != nil {
		log.Printf("core_handler: publish %s for %s: %v", subject, stationID, err)
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
	h.dbg("data: subject=%s body_size=%d from=%s", p.Subject, len(p.Body), env.Src.Station)
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
	case protocol.SubjectTagVerifyRequest:
		var req protocol.TagVerifyRequest
		if err := json.Unmarshal(p.Body, &req); err != nil {
			log.Printf("core_handler: decode tag verify request body: %v", err)
			return
		}
		h.handleTagVerifyRequest(env, &req)
	case protocol.SubjectCatalogStylesRequest, protocol.SubjectCatalogBlueprintsRequest:
		h.handleCatalogBlueprintsRequest(env)
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

	h.replyData(env, protocol.SubjectEdgeRegistered,
		&protocol.EdgeRegistered{StationID: p.StationID, Message: "registered"})
	h.dbg("reply published: subject=edge.registered station=%s", p.StationID)
}

func (h *CoreHandler) handleEdgeHeartbeat(env *protocol.Envelope, p *protocol.EdgeHeartbeat) {
	isNew, err := h.db.UpdateHeartbeat(p.StationID)
	if err != nil {
		log.Printf("core_handler: update heartbeat for %s: %v", p.StationID, err)
		return
	}

	h.replyData(env, protocol.SubjectEdgeHeartbeatAck,
		&protocol.EdgeHeartbeatAck{StationID: p.StationID, ServerTS: time.Now().UTC()})

	if isNew {
		log.Printf("core_handler: unregistered edge %s detected via heartbeat, requesting registration", p.StationID)
		h.sendData(protocol.SubjectEdgeRegisterRequest, p.StationID,
			&protocol.EdgeRegisterRequest{StationID: p.StationID, Reason: "unregistered edge detected"})
	}
}

func (h *CoreHandler) handleNodeListRequest(env *protocol.Envelope) {
	stationID := env.Src.Station
	// Try station-scoped node list first; fall back to all nodes if none assigned
	nodes, err := h.db.ListNodesForStation(stationID)
	stationScoped := err == nil && len(nodes) > 0
	if !stationScoped {
		nodes, err = h.db.ListNodes()
	}
	if err != nil {
		log.Printf("core_handler: list nodes for %s: %v", stationID, err)
		return
	}

	var infos []protocol.NodeInfo
	if stationScoped {
		// Station-scoped: include all returned nodes; use dot-notation for NGRP children
		for _, n := range nodes {
			name := n.Name
			if n.ParentID != nil && !n.IsSynthetic && n.ParentName != "" {
				name = n.ParentName + "." + n.Name
			}
			infos = append(infos, protocol.NodeInfo{Name: name, NodeType: n.NodeTypeCode})
		}
	} else {
		// Fallback (all nodes): include top-level and explicitly-assigned NGRP children
		// Build a map of parent type codes for NGRP check
		nodeMap := make(map[int64]*store.Node, len(nodes))
		for _, n := range nodes {
			nodeMap[n.ID] = n
		}
		for _, n := range nodes {
			if n.ParentID == nil {
				infos = append(infos, protocol.NodeInfo{Name: n.Name, NodeType: n.NodeTypeCode})
			} else if !n.IsSynthetic {
				if parent, ok := nodeMap[*n.ParentID]; ok && parent.NodeTypeCode == "NGRP" {
					infos = append(infos, protocol.NodeInfo{Name: parent.Name + "." + n.Name, NodeType: n.NodeTypeCode})
				}
			}
		}
	}
	h.replyData(env, protocol.SubjectNodeListResponse, &protocol.NodeListResponse{Nodes: infos})
	log.Printf("core_handler: sent node list (%d nodes) to %s", len(infos), env.Src.Station)
}

// Order message handlers delegate to the dispatcher.

func (h *CoreHandler) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	log.Printf("core_handler: order request from %s: uuid=%s type=%s", env.Src.Station, p.OrderUUID, p.OrderType)
	h.dbg("-> order_request from=%s uuid=%s type=%s", env.Src.Station, p.OrderUUID, p.OrderType)
	h.dispatcher.HandleOrderRequest(env, p)
}

func (h *CoreHandler) HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel) {
	log.Printf("core_handler: order cancel from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dbg("-> order_cancel from=%s uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderCancel(env, p)
}

func (h *CoreHandler) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	log.Printf("core_handler: delivery receipt from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dbg("-> order_receipt from=%s uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderReceipt(env, p)
}

func (h *CoreHandler) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	log.Printf("core_handler: redirect from %s: uuid=%s -> %s", env.Src.Station, p.OrderUUID, p.NewDeliveryNode)
	h.dbg("-> order_redirect from=%s uuid=%s new_dest=%s", env.Src.Station, p.OrderUUID, p.NewDeliveryNode)
	h.dispatcher.HandleOrderRedirect(env, p)
}

func (h *CoreHandler) HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill) {
	log.Printf("core_handler: storage waybill from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dbg("-> storage_waybill from=%s uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderStorageWaybill(env, p)
}

func (h *CoreHandler) handleProductionReport(env *protocol.Envelope, rpt *protocol.ProductionReport) {
	log.Printf("core_handler: production report from %s: %d entries", rpt.StationID, len(rpt.Reports))
	accepted := 0
	for _, entry := range rpt.Reports {
		if entry.CatID == "" || entry.Count <= 0 {
			continue
		}
		if err := h.db.IncrementProduced(entry.CatID, entry.Count); err != nil {
			log.Printf("core_handler: increment produced %s: %v", entry.CatID, err)
			continue
		}
		if err := h.db.LogProduction(entry.CatID, rpt.StationID, entry.Count); err != nil {
			log.Printf("core_handler: log production %s: %v", entry.CatID, err)
		}
		accepted++
	}

	h.replyData(env, protocol.SubjectProductionReportAck,
		&protocol.ProductionReportAck{StationID: rpt.StationID, Accepted: accepted})
}

func (h *CoreHandler) staleEdgeLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			staleIDs, err := h.db.MarkStaleEdges(180 * time.Second)
			if err != nil {
				log.Printf("core_handler: mark stale edges: %v", err)
				continue
			}
			if len(staleIDs) > 0 {
				h.dbg("stale edge check: %d stale", len(staleIDs))
			}
			for _, sid := range staleIDs {
				log.Printf("core_handler: edge %s marked stale, sending notification", sid)
				h.sendStaleNotification(sid)
			}
		}
	}
}

func (h *CoreHandler) handleTagVerifyRequest(env *protocol.Envelope, req *protocol.TagVerifyRequest) {
	log.Printf("core_handler: tag verify from %s: uuid=%s tag=%s", env.Src.Station, req.OrderUUID, req.TagID)

	result := h.db.VerifyTag(req.OrderUUID, req.TagID, req.Location)
	if !result.Match {
		log.Printf("core_handler: tag mismatch for order %s: expected=%s (proceeding best-effort)", req.OrderUUID, result.Expected)
	}

	h.sendTagVerifyResponse(env, &protocol.TagVerifyResponse{
		OrderUUID: req.OrderUUID,
		Match:     result.Match,
		Expected:  result.Expected,
		Detail:    result.Detail,
	})
}

func (h *CoreHandler) sendTagVerifyResponse(env *protocol.Envelope, resp *protocol.TagVerifyResponse) {
	h.replyData(env, protocol.SubjectTagVerifyResponse, resp)
}

func (h *CoreHandler) handleCatalogBlueprintsRequest(env *protocol.Envelope) {
	log.Printf("core_handler: catalog blueprints request from %s", env.Src.Station)
	blueprints, err := h.db.ListBlueprints()
	if err != nil {
		log.Printf("core_handler: list blueprints for catalog: %v", err)
		return
	}
	infos := make([]protocol.CatalogBlueprintInfo, len(blueprints))
	for i, bp := range blueprints {
		infos[i] = protocol.CatalogBlueprintInfo{
			ID: bp.ID, Name: bp.Code, Code: bp.Code,
			Description: bp.Description,
			UOPCapacity: bp.UOPCapacity,
		}
	}
	h.replyData(env, protocol.SubjectCatalogBlueprintsResponse, &protocol.CatalogBlueprintsResponse{Blueprints: infos})
	log.Printf("core_handler: sent blueprint catalog (%d blueprints) to %s", len(infos), env.Src.Station)
}

func (h *CoreHandler) sendStaleNotification(stationID string) {
	h.sendData(protocol.SubjectEdgeStale, stationID,
		&protocol.EdgeStale{StationID: stationID, Message: "heartbeat timeout — marked stale by core"})
}
