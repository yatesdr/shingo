package messaging

import (
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/nodes"
)

type coreDataResponder interface {
	dbg(format string, args ...any)
	replyData(env *protocol.Envelope, subject string, payload any)
	sendData(subject, stationID string, payload any)
}

// ThresholdMonitor is the minimal surface CoreDataService needs from the
// engine's threshold monitor. The monitor is notified directly by the
// Kafka delta handlers (OnBinUOPDelta, OnBucketApplied) which carry the
// payload code and delta — no DB queries on the hot path.
type ThresholdMonitor interface {
	OnRegistryChanges(changes []demands.RegistryChange)
	OnBinUOPDelta(payloadCode string, delta int)
	OnBucketApplied(station string, nodeID int64, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason)
}

type CoreDataService struct {
	db               *store.DB
	tagVerify        *service.TagVerifyService
	inventoryDelta   *service.InventoryDeltaService
	resp             coreDataResponder
	thresholdMonitor ThresholdMonitor
}

// SetThresholdMonitor wires the engine's threshold-monitor for
// SyncRegistry change notifications and bucket-applied events.
// Optional; may be nil — tests that don't exercise the UOP-threshold
// path can skip it.
func (s *CoreDataService) SetThresholdMonitor(tm ThresholdMonitor) {
	s.thresholdMonitor = tm
}

// NewCoreDataService constructs a CoreDataService. The TagVerifyService is
// built internally from the same *store.DB so the constructor signature
// stays minimal. Subject-router registration is the composition root's
// responsibility — it calls RegisterSubject against this service's
// HandleX methods explicitly, matching the EdgeHandler wiring pattern
// (cmd/shingoedge/main.go). Keeping the dispatch table at the
// composition root rather than buried in this constructor means a
// reader can see every Subject Core handles by grepping cmd/shingocore.
func NewCoreDataService(db *store.DB, resp coreDataResponder) *CoreDataService {
	return &CoreDataService{
		db:             db,
		tagVerify:      service.NewTagVerifyService(db),
		inventoryDelta: service.NewInventoryDeltaService(db, service.NewBinManifestService(db)),
		resp:           resp,
	}
}

// HandleBinUOPDelta routes a Phase 1 inventory delta envelope to the
// InventoryDeltaService. Errors land in the log loud (no Edge reply
// channel exists for these — they're fire-and-forget from Edge's
// outbox); a missing target bin or payload mismatch is the
// dead-letter signal. Replays (already-applied SequenceID) are
// silently dropped at the dedup step.
//
// Core applies deltas authoritatively against bins.uop_remaining;
// Edge's runtime cache trails authoritative state via the reconciler.
func (s *CoreDataService) HandleBinUOPDelta(env *protocol.Envelope, d *protocol.BinUOPDelta) {
	// Edge sets the station from its own identity at outbox time. Trust
	// the envelope source for routing — preserves the two-edge case
	// where d.Station is set on Edge before the message hits the wire
	// but we still want to attribute by the verified envelope source.
	if d.Station == "" {
		d.Station = env.Src.Station
	}
	if err := s.inventoryDelta.ApplyBinUOPDelta(d); err != nil {
		if errors.Is(err, service.ErrInventoryDeltaSkipped) {
			s.resp.dbg("bin_uop_delta replay station=%s bin=%d seq=%d — already applied",
				d.Station, d.BinID, d.SequenceID)
			return
		}
		log.Printf("core_handler: apply BinUOPDelta station=%s bin=%d seq=%d delta=%d reason=%s: %v",
			d.Station, d.BinID, d.SequenceID, d.Delta, d.Reason, err)
		return
	}
	s.resp.dbg("bin_uop_delta applied station=%s bin=%d seq=%d delta=%d reason=%s",
		d.Station, d.BinID, d.SequenceID, d.Delta, d.Reason)

	// Notify the UOP-threshold monitor so the delta is applied to the
	// cached UOP total and thresholds are checked. The monitor does
	// zero DB queries on this path — it applies the delta directly
	// to its in-memory cache.
	if s.thresholdMonitor != nil && d.PayloadCode != "" {
		s.thresholdMonitor.OnBinUOPDelta(d.PayloadCode, d.Delta)
	}
}

// HandleLinesideBucketDelta routes a Phase 1 inventory delta envelope
// to the InventoryDeltaService. Same dead-letter / authoritative-write notes
// as HandleBinUOPDelta apply. Manual-swap nodes never emit bucket
// deltas (no PLC) — a delta arriving from a manual-swap node would
// indicate an Edge bug.
func (s *CoreDataService) HandleLinesideBucketDelta(env *protocol.Envelope, d *protocol.LinesideBucketDelta) {
	if d.Station == "" {
		d.Station = env.Src.Station
	}
	if err := s.inventoryDelta.ApplyLinesideBucketDelta(d); err != nil {
		if errors.Is(err, service.ErrInventoryDeltaSkipped) {
			s.resp.dbg("lineside_bucket_delta replay station=%s node=%d part=%q seq=%d — already applied",
				d.Station, d.NodeID, d.PartNumber, d.SequenceID)
			return
		}
		log.Printf("core_handler: apply LinesideBucketDelta station=%s node=%d part=%q seq=%d delta=%d reason=%s: %v",
			d.Station, d.NodeID, d.PartNumber, d.SequenceID, d.Delta, d.Reason, err)
		return
	}
	s.resp.dbg("lineside_bucket_delta applied station=%s node=%d part=%q seq=%d delta=%d reason=%s",
		d.Station, d.NodeID, d.PartNumber, d.SequenceID, d.Delta, d.Reason)

	// Notify the UOP-threshold monitor so a bucket drain or capture
	// re-evaluates loop totals. The monitor's debounce + opt-in gating
	// inside is what keeps this from being noisy. Empty payload_code is
	// fine — the monitor short-circuits on unknown payload.
	if s.thresholdMonitor != nil {
		s.thresholdMonitor.OnBucketApplied(d.Station, d.NodeID, d.PayloadCode, d.Delta, d.Reason)
	}
}

// HandleCountGroupAck records an edge's response to a prior CountGroupCommand.
// One audit row per ack — combined with the transition-side row emitted by
// countgroup_wiring.go, this gives end-to-end forensics: core saw X, edge
// wrote Y, PLC took Z ms to ack (or timed out).
func (s *CoreDataService) HandleCountGroupAck(env *protocol.Envelope, ack *protocol.CountGroupAck) {
	log.Printf("core_handler: countgroup ack from=%s group=%s outcome=%s latency=%dms corr=%s",
		env.Src.Station, ack.Group, ack.Outcome, ack.AckLatencyMs, ack.CorrelationID)
	detail := fmt.Sprintf("group=%s outcome=%s latency_ms=%d corr=%s station=%s",
		ack.Group, ack.Outcome, ack.AckLatencyMs, ack.CorrelationID, env.Src.Station)
	if err := s.db.AppendAudit("countgroup_ack", 0, string(ack.Outcome), "", detail, env.Src.Station); err != nil {
		log.Printf("core_handler: countgroup ack audit: %v", err)
	}
}

func (s *CoreDataService) HandleEdgeRegister(env *protocol.Envelope, p *protocol.EdgeRegister) {
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

func (s *CoreDataService) HandleEdgeHeartbeat(env *protocol.Envelope, p *protocol.EdgeHeartbeat) {
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

func (s *CoreDataService) HandleNodeListRequest(env *protocol.Envelope) {
	stationID := env.Src.Station
	nodeList, err := s.db.ListNodesForStation(stationID)
	stationScoped := err == nil && len(nodeList) > 0
	if !stationScoped {
		nodeList, err = s.db.ListNodes()
	}
	if err != nil {
		log.Printf("core_handler: list nodes for %s: %v", stationID, err)
		return
	}

	// parentType resolves the parent's NodeTypeCode without assuming the
	// parent sits in the current result slice. Station-scoped queries
	// only return rows assigned to the station, so a storage slot's
	// LANE parent typically won't be included — a single targeted Get
	// is the cheapest correct lookup.
	parentType := func(parentID *int64) string {
		if parentID == nil {
			return ""
		}
		p, err := s.db.GetNode(*parentID)
		if err != nil || p == nil {
			return ""
		}
		return p.NodeTypeCode
	}

	var infos []protocol.NodeInfo
	if stationScoped {
		for _, n := range nodeList {
			name := n.Name
			if n.ParentID != nil && !n.IsSynthetic && n.ParentName != "" {
				name = n.ParentName + "." + n.Name
			}
			infos = append(infos, protocol.NodeInfo{
				Name:           name,
				NodeType:       n.NodeTypeCode,
				ParentNodeType: parentType(n.ParentID),
			})
		}
	} else {
		nodeMap := make(map[int64]*nodes.Node, len(nodeList))
		for _, n := range nodeList {
			nodeMap[n.ID] = n
		}
		for _, n := range nodeList {
			if n.ParentID == nil {
				infos = append(infos, protocol.NodeInfo{Name: n.Name, NodeType: n.NodeTypeCode})
			} else if !n.IsSynthetic {
				if parent, ok := nodeMap[*n.ParentID]; ok && parent.NodeTypeCode == "NGRP" {
					infos = append(infos, protocol.NodeInfo{
						Name:           parent.Name + "." + n.Name,
						NodeType:       n.NodeTypeCode,
						ParentNodeType: parent.NodeTypeCode,
					})
				}
			}
		}
	}
	s.resp.replyData(env, protocol.SubjectNodeListResponse, &protocol.NodeListResponse{Nodes: infos})
	log.Printf("core_handler: sent node list (%d nodes) to %s", len(infos), env.Src.Station)
}

func (s *CoreDataService) HandleProductionReport(env *protocol.Envelope, rpt *protocol.ProductionReport) {
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

func (s *CoreDataService) HandleTagVerifyRequest(env *protocol.Envelope, req *protocol.TagVerifyRequest) {
	log.Printf("core_handler: tag verify from %s: uuid=%s tag=%s", env.Src.Station, req.OrderUUID, req.TagID)

	result := s.tagVerify.VerifyTag(req.OrderUUID, req.TagID, req.Location)
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

func (s *CoreDataService) HandleCatalogPayloadsRequest(env *protocol.Envelope) {
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

func (s *CoreDataService) HandleNodeStateRequest(env *protocol.Envelope, req *protocol.NodeStateRequest) {
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

func (s *CoreDataService) HandleOrderStatusRequest(env *protocol.Envelope, req *protocol.OrderStatusRequest) {
	resp := &protocol.OrderStatusResponse{Orders: make([]protocol.OrderStatusSnapshot, 0, len(req.OrderUUIDs))}
	for _, orderUUID := range req.OrderUUIDs {
		snap := protocol.OrderStatusSnapshot{OrderUUID: orderUUID}
		order, err := s.db.GetOrderByUUID(orderUUID)
		if err == nil && order != nil {
			snap.Found = true
			snap.Status = string(order.Status)
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

func (s *CoreDataService) HandleClaimSync(env *protocol.Envelope, sync *protocol.ClaimSync) {
	stationID := sync.StationID
	if stationID == "" {
		stationID = env.Src.Station
	}
	log.Printf("core_handler: claim sync from %s: %d claims", stationID, len(sync.Claims))

	// Convert protocol entries to store entries, warning when a consume
	// claim targets a node that isn't LANE-parented — HandleKanbanDemand
	// will never fire a consume signal for such nodes (see isStorageSlot
	// in wiring_kanban.go), so the registry row is inert and usually
	// means an Edge-UI validation gap. Warn-don't-reject keeps this a
	// belt-and-suspenders check alongside the Edge-side 400.
	var entries []demands.RegistryEntry
	for _, c := range sync.Claims {
		if c.Role == protocol.ClaimRoleConsume {
			if node, err := s.db.GetNodeByDotName(c.CoreNodeName); err == nil && node != nil && node.ParentID != nil {
				if parent, err := s.db.GetNode(*node.ParentID); err == nil && parent != nil && parent.NodeTypeCode != "LANE" {
					log.Printf("core_handler: consume claim from %s targets %s (parent node_type=%s, not LANE) — demand signals will be suppressed by wiring_kanban", stationID, c.CoreNodeName, parent.NodeTypeCode)
				}
			}
		}
		for _, pc := range c.AllowedPayloadCodes {
			// UOP-threshold replenishment: pull per-payload threshold
			// from the ClaimSync map. Omitted/zero means "Core does
			// not monitor this pair" (legacy bin-count at Edge).
			thr := c.PayloadThresholds[pc]
			entries = append(entries, demands.RegistryEntry{
				StationID:             stationID,
				CoreNodeName:          c.CoreNodeName,
				Role:                  c.Role,
				PayloadCode:           pc,
				OutboundDest:          c.OutboundDestination,
				ReplenishUOPThreshold: thr,
			})
		}
	}

	changes, err := s.db.SyncDemandRegistry(stationID, entries)
	if err != nil {
		log.Printf("core_handler: sync demand registry for %s: %v", stationID, err)
		return
	}
	log.Printf("core_handler: demand registry updated for %s: %d entries (%d threshold changes)", stationID, len(entries), len(changes))

	// Reset threshold-monitor debounce for any (loader, payload) whose
	// threshold value moved, so the new value engages immediately
	// instead of waiting out the debounce window.
	if s.thresholdMonitor != nil && len(changes) > 0 {
		s.thresholdMonitor.OnRegistryChanges(changes)
	}
}
