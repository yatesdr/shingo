package messaging

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingocore/store"
)

type coreDataResponder interface {
	dbg(format string, args ...any)
	replyData(env *protocol.Envelope, subject string, payload any)
	sendData(subject, stationID string, payload any)
}

type CoreDataService struct {
	db   *store.DB
	resp coreDataResponder
}

func newCoreDataService(db *store.DB, resp coreDataResponder) *CoreDataService {
	return &CoreDataService{db: db, resp: resp}
}

func (s *CoreDataService) Handle(env *protocol.Envelope, p *protocol.Data) {
	s.resp.dbg("data: subject=%s body_size=%d from=%s", p.Subject, len(p.Body), env.Src.Station)
	switch p.Subject {
	case protocol.SubjectEdgeRegister:
		var reg protocol.EdgeRegister
		if err := json.Unmarshal(p.Body, &reg); err != nil {
			log.Printf("core_handler: decode edge register body: %v", err)
			return
		}
		s.handleEdgeRegister(env, &reg)
	case protocol.SubjectEdgeHeartbeat:
		var hb protocol.EdgeHeartbeat
		if err := json.Unmarshal(p.Body, &hb); err != nil {
			log.Printf("core_handler: decode edge heartbeat body: %v", err)
			return
		}
		s.handleEdgeHeartbeat(env, &hb)
	case protocol.SubjectNodeListRequest:
		s.handleNodeListRequest(env)
	case protocol.SubjectProductionReport:
		var rpt protocol.ProductionReport
		if err := json.Unmarshal(p.Body, &rpt); err != nil {
			log.Printf("core_handler: decode production report body: %v", err)
			return
		}
		s.handleProductionReport(env, &rpt)
	case protocol.SubjectTagVerifyRequest:
		var req protocol.TagVerifyRequest
		if err := json.Unmarshal(p.Body, &req); err != nil {
			log.Printf("core_handler: decode tag verify request body: %v", err)
			return
		}
		s.handleTagVerifyRequest(env, &req)
	case protocol.SubjectCatalogPayloadsRequest:
		s.handleCatalogPayloadsRequest(env)
	case protocol.SubjectNodeStateRequest:
		var req protocol.NodeStateRequest
		if err := json.Unmarshal(p.Body, &req); err != nil {
			log.Printf("core_handler: decode node state request body: %v", err)
			return
		}
		s.handleNodeStateRequest(env, &req)
	case protocol.SubjectOrderStatusRequest:
		var req protocol.OrderStatusRequest
		if err := json.Unmarshal(p.Body, &req); err != nil {
			log.Printf("core_handler: decode order status request body: %v", err)
			return
		}
		s.handleOrderStatusRequest(env, &req)
	case protocol.SubjectClaimSync:
		var sync protocol.ClaimSync
		if err := json.Unmarshal(p.Body, &sync); err != nil {
			log.Printf("core_handler: decode claim sync body: %v", err)
			return
		}
		s.handleClaimSync(env, &sync)
	case protocol.SubjectCountGroupAck:
		var ack protocol.CountGroupAck
		if err := json.Unmarshal(p.Body, &ack); err != nil {
			log.Printf("core_handler: decode countgroup ack body: %v", err)
			return
		}
		s.handleCountGroupAck(env, &ack)
	default:
		log.Printf("core_handler: unhandled data subject: %s", p.Subject)
	}
}

// handleCountGroupAck records an edge's response to a prior CountGroupCommand.
// One audit row per ack — combined with the transition-side row emitted by
// countgroup_wiring.go, this gives end-to-end forensics: core saw X, edge
// wrote Y, PLC took Z ms to ack (or timed out).
func (s *CoreDataService) handleCountGroupAck(env *protocol.Envelope, ack *protocol.CountGroupAck) {
	log.Printf("core_handler: countgroup ack from=%s group=%s outcome=%s latency=%dms corr=%s",
		env.Src.Station, ack.Group, ack.Outcome, ack.AckLatencyMs, ack.CorrelationID)
	detail := fmt.Sprintf("group=%s outcome=%s latency_ms=%d corr=%s station=%s",
		ack.Group, ack.Outcome, ack.AckLatencyMs, ack.CorrelationID, env.Src.Station)
	if err := s.db.AppendAudit("countgroup_ack", 0, ack.Outcome, "", detail, env.Src.Station); err != nil {
		log.Printf("core_handler: countgroup ack audit: %v", err)
	}
}

func (s *CoreDataService) handleEdgeRegister(env *protocol.Envelope, p *protocol.EdgeRegister) {
	log.Printf("core_handler: edge registered: %s (hostname=%s, version=%s, lines=%v)",
		p.StationID, p.Hostname, p.Version, p.LineIDs)

	if err := s.db.RegisterEdge(p.StationID, p.Hostname, p.Version, p.LineIDs); err != nil {
		log.Printf("core_handler: register edge %s: %v", p.StationID, err)
		return
	}

	s.resp.replyData(env, protocol.SubjectEdgeRegistered,
		&protocol.EdgeRegistered{StationID: p.StationID, Message: "registered"})
	s.resp.dbg("reply published: subject=edge.registered station=%s", p.StationID)
}

func (s *CoreDataService) handleEdgeHeartbeat(env *protocol.Envelope, p *protocol.EdgeHeartbeat) {
	isNew, err := s.db.UpdateHeartbeat(p.StationID)
	if err != nil {
		log.Printf("core_handler: update heartbeat for %s: %v", p.StationID, err)
		return
	}

	s.resp.replyData(env, protocol.SubjectEdgeHeartbeatAck,
		&protocol.EdgeHeartbeatAck{StationID: p.StationID, ServerTS: time.Now().UTC()})

	if isNew {
		log.Printf("core_handler: unregistered edge %s detected via heartbeat, requesting registration", p.StationID)
		s.resp.sendData(protocol.SubjectEdgeRegisterRequest, p.StationID,
			&protocol.EdgeRegisterRequest{StationID: p.StationID, Reason: "unregistered edge detected"})
	}
}

func (s *CoreDataService) handleNodeListRequest(env *protocol.Envelope) {
	stationID := env.Src.Station
	nodes, err := s.db.ListNodesForStation(stationID)
	stationScoped := err == nil && len(nodes) > 0
	if !stationScoped {
		nodes, err = s.db.ListNodes()
	}
	if err != nil {
		log.Printf("core_handler: list nodes for %s: %v", stationID, err)
		return
	}

	var infos []protocol.NodeInfo
	if stationScoped {
		for _, n := range nodes {
			name := n.Name
			if n.ParentID != nil && !n.IsSynthetic && n.ParentName != "" {
				name = n.ParentName + "." + n.Name
			}
			infos = append(infos, protocol.NodeInfo{Name: name, NodeType: n.NodeTypeCode})
		}
	} else {
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
	s.resp.replyData(env, protocol.SubjectNodeListResponse, &protocol.NodeListResponse{Nodes: infos})
	log.Printf("core_handler: sent node list (%d nodes) to %s", len(infos), env.Src.Station)
}

func (s *CoreDataService) handleProductionReport(env *protocol.Envelope, rpt *protocol.ProductionReport) {
	log.Printf("core_handler: production report from %s: %d entries", rpt.StationID, len(rpt.Reports))
	accepted := 0
	for _, entry := range rpt.Reports {
		if entry.CatID == "" || entry.Count <= 0 {
			continue
		}
		if err := s.db.IncrementProduced(entry.CatID, entry.Count); err != nil {
			log.Printf("core_handler: increment produced %s: %v", entry.CatID, err)
			continue
		}
		if err := s.db.LogProduction(entry.CatID, rpt.StationID, entry.Count); err != nil {
			log.Printf("core_handler: log production %s: %v", entry.CatID, err)
		}
		accepted++
	}

	s.resp.replyData(env, protocol.SubjectProductionReportAck,
		&protocol.ProductionReportAck{StationID: rpt.StationID, Accepted: accepted})
}

func (s *CoreDataService) handleTagVerifyRequest(env *protocol.Envelope, req *protocol.TagVerifyRequest) {
	log.Printf("core_handler: tag verify from %s: uuid=%s tag=%s", env.Src.Station, req.OrderUUID, req.TagID)

	result := s.db.VerifyTag(req.OrderUUID, req.TagID, req.Location)
	if !result.Match {
		log.Printf("core_handler: tag mismatch for order %s: expected=%s (proceeding best-effort)", req.OrderUUID, result.Expected)
	}

	s.resp.replyData(env, protocol.SubjectTagVerifyResponse, &protocol.TagVerifyResponse{
		OrderUUID: req.OrderUUID,
		Match:     result.Match,
		Expected:  result.Expected,
		Detail:    result.Detail,
	})
}

func (s *CoreDataService) handleCatalogPayloadsRequest(env *protocol.Envelope) {
	log.Printf("core_handler: catalog payloads request from %s", env.Src.Station)
	payloads, err := s.db.ListPayloads()
	if err != nil {
		log.Printf("core_handler: list payloads for catalog: %v", err)
		return
	}
	infos := make([]protocol.CatalogPayloadInfo, len(payloads))
	for i, p := range payloads {
		infos[i] = protocol.CatalogPayloadInfo{
			ID: p.ID, Name: p.Code, Code: p.Code,
			Description: p.Description,
			UOPCapacity: p.UOPCapacity,
		}
	}
	s.resp.replyData(env, protocol.SubjectCatalogPayloadsResponse, &protocol.CatalogPayloadsResponse{Payloads: infos})
	log.Printf("core_handler: sent payload catalog (%d payloads) to %s", len(infos), env.Src.Station)
}

func (s *CoreDataService) handleNodeStateRequest(env *protocol.Envelope, req *protocol.NodeStateRequest) {
	log.Printf("core_handler: node state request from %s: %d nodes", env.Src.Station, len(req.Nodes))
	entries := make([]protocol.NodeStateEntry, 0, len(req.Nodes))
	for _, name := range req.Nodes {
		entry := protocol.NodeStateEntry{Name: name}
		node, err := s.db.GetNodeByName(name)
		if err != nil {
			entries = append(entries, entry)
			continue
		}
		bins, err := s.db.ListBinsByNode(node.ID)
		if err != nil {
			entries = append(entries, entry)
			continue
		}
		entry.BinCount = len(bins)
		entry.Occupied = len(bins) > 0
		for _, b := range bins {
			if entry.PayloadCode == "" {
				entry.PayloadCode = b.PayloadCode
			}
			if b.ClaimedBy != nil {
				entry.Claimed = true
			}
		}
		entries = append(entries, entry)
	}
	s.resp.replyData(env, protocol.SubjectNodeStateResponse, &protocol.NodeStateResponse{Nodes: entries})
	log.Printf("core_handler: sent node state (%d entries) to %s", len(entries), env.Src.Station)
}

func (s *CoreDataService) handleOrderStatusRequest(env *protocol.Envelope, req *protocol.OrderStatusRequest) {
	resp := &protocol.OrderStatusResponse{Orders: make([]protocol.OrderStatusSnapshot, 0, len(req.OrderUUIDs))}
	for _, orderUUID := range req.OrderUUIDs {
		snap := protocol.OrderStatusSnapshot{OrderUUID: orderUUID}
		order, err := s.db.GetOrderByUUID(orderUUID)
		if err == nil && order != nil {
			snap.Found = true
			snap.Status = order.Status
			snap.StationID = order.StationID
			snap.SourceNode = order.SourceNode
			snap.DeliveryNode = order.DeliveryNode
			snap.VendorOrderID = order.VendorOrderID
			snap.ErrorDetail = order.ErrorDetail
		}
		resp.Orders = append(resp.Orders, snap)
	}
	s.resp.replyData(env, protocol.SubjectOrderStatusResponse, resp)
}

func (s *CoreDataService) handleClaimSync(env *protocol.Envelope, sync *protocol.ClaimSync) {
	stationID := sync.StationID
	if stationID == "" {
		stationID = env.Src.Station
	}
	log.Printf("core_handler: claim sync from %s: %d claims", stationID, len(sync.Claims))

	// Convert protocol entries to store entries
	var entries []store.DemandRegistryEntry
	for _, c := range sync.Claims {
		for _, pc := range c.AllowedPayloadCodes {
			entries = append(entries, store.DemandRegistryEntry{
				StationID:    stationID,
				CoreNodeName: c.CoreNodeName,
				Role:         c.Role,
				PayloadCode:  pc,
				OutboundDest: c.OutboundDestination,
			})
		}
	}

	if err := s.db.SyncDemandRegistry(stationID, entries); err != nil {
		log.Printf("core_handler: sync demand registry for %s: %v", stationID, err)
		return
	}
	log.Printf("core_handler: demand registry updated for %s: %d entries", stationID, len(entries))
}
