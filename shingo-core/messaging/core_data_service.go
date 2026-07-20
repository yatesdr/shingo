package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/downtime"
	"shingocore/store/heartbeat"
	"shingocore/store/nodes"
)

// heartbeatRetentionDays is the cell_part_events retention window (plan §12).
const heartbeatRetentionDays = 90

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
	OnThresholdChanges(changes []demands.RegistryChange)
	OnBinUOPDelta(payloadCode string, delta int)
	OnBucketApplied(station, coreNodeName, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason)
	// Resync re-engages a station's demand_registry bindings on (re)connect, so a
	// threshold seeded after the startup sweep fires without a Core restart.
	Resync(stationID string)
}

type CoreDataService struct {
	db               *store.DB
	tagVerify        *service.TagVerifyService
	inventoryDelta   *service.InventoryDeltaService
	resp             coreDataResponder
	thresholdMonitor ThresholdMonitor
	// tickCh buffers production.tick projections for the async worker started
	// by StartHeartbeatProjection. HandleProductionTick only enqueues
	// (non-blocking), so a slow/locked cell_part_events table can never
	// back-pressure the inventory hot path (plan §12).
	tickCh chan heartbeat.PartEvent
	// downtimeCh buffers downtime event projections for the async worker
	// started by StartDowntimeProjection (G9). Mirrors tickCh pattern.
	downtimeCh chan downtime.DowntimeEvent
	// cellTickEmitter, if set, fires after a tick is projected so the
	// composition root can fan it out — the SSE cell-heartbeat broadcast
	// (Phase E). Optional; nil in tests and headless runs. Set once before
	// StartHeartbeatProjection, so the worker reads it race-free.
	cellTickEmitter func(station string, processID, styleID int64, recordedAt time.Time)
}

// SetThresholdMonitor wires the engine's threshold-monitor for
// SyncRegistry change notifications and bucket-applied events.
// Optional; may be nil — tests that don't exercise the UOP-threshold
// path can skip it.
func (s *CoreDataService) SetThresholdMonitor(tm ThresholdMonitor) {
	s.thresholdMonitor = tm
}

// SetCellTickEmitter wires a callback invoked after each production.tick is
// projected into cell_part_events (Phase E). The composition root points it at
// the engine event bus, which SetupEngineListeners rebroadcasts as the SSE
// cell-heartbeat. Optional; may be nil. Set before StartHeartbeatProjection.
func (s *CoreDataService) SetCellTickEmitter(fn func(station string, processID, styleID int64, recordedAt time.Time)) {
	s.cellTickEmitter = fn
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
		tickCh:         make(chan heartbeat.PartEvent, 4096),
		downtimeCh:     make(chan downtime.DowntimeEvent, 1024),
	}
}

// StartHeartbeatProjection launches the async cell_part_events projection
// worker and the monthly-partition manager (plan §12). Call once at the
// composition root after subject registration. The projection is decoupled
// from inventory: HandleProductionTick only enqueues; this worker does the
// INSERT, so a slow/locked projection table never back-pressures the delta
// hot path. Goroutines live for the process lifetime (daemon model).
func (s *CoreDataService) StartHeartbeatProjection() {
	if err := s.db.EnsureHeartbeatPartitions(clock.Now().UTC()); err != nil {
		log.Printf("core_handler: ensure heartbeat partitions at boot: %v", err)
	}
	go func() {
		for e := range s.tickCh {
			if err := s.db.InsertCellPartEvent(e); err != nil {
				log.Printf("core_handler: project cell_part_event cell=%s edge_id=%d: %v", e.CellID, e.EdgeSnapshotID, err)
				continue
			}
			if s.cellTickEmitter != nil {
				s.cellTickEmitter(e.CellID, e.ProcessID, e.StyleID, e.RecordedAt)
			}
		}
	}()
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for range t.C {
			now := clock.Now().UTC()
			if err := s.db.EnsureHeartbeatPartitions(now); err != nil {
				log.Printf("core_handler: ensure heartbeat partitions: %v", err)
			}
			if dropped, err := s.db.DropOldHeartbeatPartitions(heartbeatRetentionDays, now); err != nil {
				log.Printf("core_handler: drop old heartbeat partitions: %v", err)
			} else if dropped > 0 {
				log.Printf("core_handler: dropped %d expired heartbeat partition(s)", dropped)
			}
		}
	}()
}

// HandleProductionTick projects an Edge production.tick (one PLC counter
// observation) into cell_part_events for the heartbeat dashboards (plan §12).
// Dedup on (station, edge_snapshot_id) is synchronous and runs first (§8 #22);
// the projection is enqueued non-blocking so it can never back-pressure the
// inventory hot path. Emits even for anomaly=="jump" (§8 #20). Because dedup
// commits before the (best-effort) projection, projection is at-most-once — an
// acceptable trade for a dashboard that is not an inventory truth source; a
// dropped/failed projection is logged, not retried.
func (s *CoreDataService) HandleProductionTick(env *protocol.Envelope, snap *protocol.CounterSnapshot) {
	station := snap.Station
	if station == "" {
		station = env.Src.Station
	}
	isNew, err := s.db.TryProductionTickDedup(station, snap.EdgeSnapshotID)
	if err != nil {
		log.Printf("core_handler: production.tick dedup station=%s edge_id=%d: %v", station, snap.EdgeSnapshotID, err)
		return
	}
	if !isNew {
		s.resp.dbg("production.tick replay station=%s edge_id=%d — already projected", station, snap.EdgeSnapshotID)
		return
	}
	ev := heartbeat.PartEvent{
		CellID:         station,
		RecordedAt:     snap.RecordedAt,
		EdgeSnapshotID: snap.EdgeSnapshotID,
		CountValue:     snap.CountValue,
		Delta:          snap.Delta,
		Anomaly:        snap.Anomaly,
		ProcessID:      snap.ProcessID,
		StyleID:        snap.StyleID,
	}
	select {
	case s.tickCh <- ev:
	default:
		log.Printf("core_handler: production.tick projection queue full, dropped station=%s edge_id=%d", station, snap.EdgeSnapshotID)
	}

	// §14 production.report retirement — BLOCKED, see Q-024. The gate
	// (isProductionTick) is ready and tested, and this isNew branch is the
	// correct, dedup-guarded placement for the IncrementProduced/LogProduction
	// calls (§14 risk #4). But IncrementProduced needs cat_id = payload_code,
	// and production.tick is emitted UPSTREAM of payload attribution
	// (plc/manager.go enqueueProductionTick has only style/process; payload is
	// attributed later in the engine wiring, where the old production_reporter
	// gets it). So cat_id is not resolvable from the tick today. Until the team
	// decides the cat_id source, production.report stays the sole writer —
	// HandleProductionReport is intentionally left active.
	if isProductionTick(snap) {
		s.resp.dbg("production.tick is a production event station=%s style=%d delta=%d (produced-count wiring blocked on cat_id source, Q-024)",
			station, snap.StyleID, snap.Delta)
	}
}

// isProductionTick reports whether a tick should increment the produced
// counter per §14's filter (Delta > 0, a real style, not an unconfirmed jump).
// Mirrors Edge's old EmitCounterDelta production guard. Ready for the §14
// retirement once the cat_id source is resolved (Q-024).
func isProductionTick(snap *protocol.CounterSnapshot) bool {
	return snap.Delta > 0 && snap.StyleID != 0 && snap.Anomaly != "jump"
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

	// §14 (Session-4 reframe): production counting retires onto bin_uop_delta.
	// We are on the APPLIED branch — ApplyBinUOPDelta returned nil, meaning the
	// delta passed its inventory_delta_dedup gate and was newly applied. A
	// Kafka redelivery returns ErrInventoryDeltaSkipped above, so the counter
	// is never double-bumped (§14 risk #4). NOT same-tx with the inventory
	// write (that lives in the uop package; counting here keeps demands
	// decoupled from inventory truth) — idempotent via the dedup gate, matching
	// the durability of the retired production.report path.
	//
	// BOTH produce and consume ticks are production, keyed by payload_code: a
	// produce tick makes the part; a consume tick draws the sub down as it's
	// produced into a downstream FG/WIP. Count the magnitude (consume delta is
	// negative). IncrementProduced is UPDATE-only, so untracked cat_ids no-op.
	if isProductionReason(d.Reason) && d.PayloadCode != "" && d.Delta != 0 {
		qty := int64(d.Delta)
		if qty < 0 {
			qty = -qty
		}
		s.resp.dbg("production via bin_uop_delta: payload=%s station=%s qty=%d reason=%s",
			d.PayloadCode, d.Station, qty, d.Reason)
		if err := s.db.IncrementProduced(d.PayloadCode, qty); err != nil {
			log.Printf("core_handler: increment produced payload=%s qty=%d: %v", d.PayloadCode, qty, err)
		}
		if err := s.db.LogProduction(d.PayloadCode, d.Station, qty); err != nil {
			log.Printf("core_handler: log production payload=%s: %v", d.PayloadCode, err)
		}
	}
}

// isProductionReason reports whether a bin_uop_delta reason represents a part
// being produced for the demand counter (§14). Both directions count, keyed by
// payload_code: produce_tick (a part is made), consume_tick and its A/B-cycling
// variant ab_fallthrough (a sub is consumed as it's produced into a downstream
// FG/WIP). Excludes capture_reduction (operator pull-to-lineside on release)
// and operator_correction (manual count fix) — material moves / corrections,
// not production throughput.
func isProductionReason(reason protocol.BinUOPDeltaReason) bool {
	switch reason {
	case protocol.ReasonProduceTick, protocol.ReasonConsumeTick, protocol.ReasonABFallthrough:
		return true
	default:
		return false
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
			s.resp.dbg("lineside_bucket_delta replay station=%s core_node=%q part=%q seq=%d — already applied",
				d.Station, d.CoreNodeName, d.PartNumber, d.SequenceID)
			return
		}
		log.Printf("core_handler: apply LinesideBucketDelta station=%s core_node=%q part=%q seq=%d delta=%d reason=%s: %v",
			d.Station, d.CoreNodeName, d.PartNumber, d.SequenceID, d.Delta, d.Reason, err)
		return
	}
	s.resp.dbg("lineside_bucket_delta applied station=%s core_node=%q part=%q seq=%d delta=%d reason=%s",
		d.Station, d.CoreNodeName, d.PartNumber, d.SequenceID, d.Delta, d.Reason)

	// Notify the UOP-threshold monitor so a bucket drain or capture
	// re-evaluates loop totals. The monitor's debounce + opt-in gating
	// inside is what keeps this from being noisy. Empty payload_code is
	// fine — the monitor short-circuits on unknown payload.
	if s.thresholdMonitor != nil {
		s.thresholdMonitor.OnBucketApplied(d.Station, d.CoreNodeName, d.PayloadCode, d.Delta, d.Reason)
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

	// Q-034: persist the auto-derived cell catalog so heartbeats populate
	// without manual setup. Additive — an old edge sends no catalog (len 0) and
	// we leave edge_cells untouched. Non-fatal: registration succeeds regardless.
	if len(p.Catalog) > 0 {
		cells := make([]store.EdgeCell, 0, len(p.Catalog))
		for _, e := range p.Catalog {
			bindings, err := json.Marshal(e.Processes)
			if err != nil {
				continue
			}
			cells = append(cells, store.EdgeCell{CellLabel: e.CellLabel, Bindings: bindings})
		}
		if err := s.db.UpsertEdgeCells(p.StationID, cells); err != nil {
			log.Printf("core_handler: upsert edge_cells for %s: %v", p.StationID, err)
		}
	}

	s.resp.replyData(env, protocol.SubjectEdgeRegistered,
		&protocol.EdgeRegistered{StationID: p.StationID, Message: "registered"})
	s.resp.dbg("reply published: subject=edge.registered station=%s", p.StationID)

	// Derive demand_registry for this station from the Core-owned loader aggregate
	// on (re)connect, so a plant configured entirely through the UI gets live
	// demand routing without an out-of-band seeddev/migrateloaders run. Idempotent
	// — SyncDemandRegistry diffs against the current rows. (The Edge sends no
	// ClaimSync; it is retired.)
	if entries, derr := s.db.BuildDemandRegistryFromAggregate(p.StationID); derr != nil {
		log.Printf("core_handler: build demand_registry for %s: %v", p.StationID, derr)
	} else if _, serr := s.db.SyncDemandRegistry(p.StationID, entries); serr != nil {
		log.Printf("core_handler: seed demand_registry for %s: %v", p.StationID, serr)
	}

	// Re-engage the threshold monitor for this station's loader bindings: the
	// monitor sweeps demand_registry once at Core startup, so a (re)connect after
	// the seed above turns the freshly-derived registry into live monitor
	// bindings. Without it a seeded UOP threshold never fires until Core restarts.
	if s.thresholdMonitor != nil {
		s.thresholdMonitor.Resync(p.StationID)
	}
}

func (s *CoreDataService) HandleEdgeHeartbeat(env *protocol.Envelope, p *protocol.EdgeHeartbeat) {
	isNew, err := s.db.UpdateHeartbeat(p.StationID)
	if err != nil {
		log.Printf("core_handler: update heartbeat for %s: %v", p.StationID, err)
		return
	}

	s.resp.replyData(env, protocol.SubjectEdgeHeartbeatAck,
		&protocol.EdgeHeartbeatAck{StationID: p.StationID, ServerTS: clock.Now().UTC()})

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
				infos = append(infos, protocol.NodeInfo{
					Name:     n.Name,
					NodeType: n.NodeTypeCode,
				})
			} else if !n.IsSynthetic {
				if parent, ok := nodeMap[*n.ParentID]; ok && parent.NodeTypeCode == protocol.NodeClassNGRP {
					infos = append(infos, protocol.NodeInfo{
						Name:           parent.Name + "." + n.Name,
						NodeType:       n.NodeTypeCode,
						ParentNodeType: parent.NodeTypeCode,
					})
				}
			}
		}
	}
	// Loader refactor cutover: include the Core-owned loader config as a sibling
	// slice so Edge's persistent cache receives it atomically with the topology.
	// Empty (and omitted on the wire) until Core authors loaders — additive.
	loaderInfos, lerr := s.db.BuildLoaderInfos()
	if lerr != nil {
		// Non-fatal: send the node list without loaders rather than nothing.
		log.Printf("core_handler: build loader infos for %s: %v", env.Src.Station, lerr)
	}
	// Payload→dunnage mapping: one query replaces the N+1 per-node
	// GetEffectiveBinTypes calls. Edge uses this to derive picker options
	// from the node's allowed payloads (claim.AllowedPayloadCodes).
	pbtPairs, pbtErr := s.db.ListPayloadBinTypeMappings()
	if pbtErr != nil {
		log.Printf("core_handler: list payload bin types for %s: %v", env.Src.Station, pbtErr)
	}
	var payloadBinTypes []protocol.PayloadBinTypeInfo
	for _, p := range pbtPairs {
		payloadBinTypes = append(payloadBinTypes, protocol.PayloadBinTypeInfo{PayloadCode: p[0], BinTypeCode: p[1]})
	}
	s.resp.replyData(env, protocol.SubjectNodeListResponse, &protocol.NodeListResponse{
		Nodes:           infos,
		Loaders:         loaderInfos,
		PayloadBinTypes: payloadBinTypes,
	})
	log.Printf("core_handler: sent node list (%d nodes, %d loaders) to %s", len(infos), len(loaderInfos), env.Src.Station)
}

func (s *CoreDataService) HandleProductionReport(env *protocol.Envelope, rpt *protocol.ProductionReport) {
	log.Printf("core_handler: production report from %s: %d entries (PARALLEL-RUN: writes disabled; new path is HandleBinUOPDelta, §14)", rpt.StationID, len(rpt.Reports))
	accepted := 0
	for _, entry := range rpt.Reports {
		if entry.CatID == "" || entry.Count <= 0 {
			continue
		}
		// §14 parallel-run (risk #3): the new bin_uop_delta path is now the
		// SOLE writer of produced_qty / production_log. IncrementProduced is
		// NOT idempotent, so we must NOT also write here — double-writing would
		// silently double the counter and the parity check would pass on both
		// being wrong. Keep the handler + ack live and LOG what this path WOULD
		// have written so Stephen can compare LOGS (not counter values) for a
		// week before the production_reporter deletion lands (Q-024-FOLLOWUP).
		log.Printf("core_handler: [production.report parallel-run] would write cat_id=%s station=%s count=%d",
			entry.CatID, rpt.StationID, entry.Count)
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
			snap.QueueReason = order.QueueReason
			snap.QueueCode = order.QueueCode
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
				if parent, err := s.db.GetNode(*node.ParentID); err == nil && parent != nil && parent.NodeTypeCode != protocol.NodeClassLANE {
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
		s.thresholdMonitor.OnThresholdChanges(changes)
	}
}

// StartDowntimeProjection launches the async downtime_events projection worker
// and partition manager (G9). Call once at the composition root after subject
// registration. Mirrors StartHeartbeatProjection: HandleDowntimeEvent enqueues,
// this worker does the INSERT.
func (s *CoreDataService) StartDowntimeProjection() {
	if err := s.db.EnsureDowntimePartitions(clock.Now().UTC()); err != nil {
		log.Printf("core_handler: ensure downtime partitions at boot: %v", err)
	}
	go func() {
		for e := range s.downtimeCh {
			if err := s.db.InsertDowntimeEvent(e); err != nil {
				log.Printf("core_handler: project downtime_event station=%s plc=%s edge_id=%d: %v", e.Station, e.PLCName, e.EdgeEventID, err)
			}
		}
	}()
}

// HandleDowntimeEvent projects an Edge downtime event into downtime_events
// for OEE availability dashboards (G9). Dedup on (station, edge_event_id)
// is synchronous and runs first; the projection is enqueued non-blocking so
// it can never back-pressure the Kafka consumer. Best-effort: a dropped
// projection is logged, not retried.
func (s *CoreDataService) HandleDowntimeEvent(env *protocol.Envelope, d *protocol.DowntimeEvent) {
	station := d.Station
	if station == "" {
		station = env.Src.Station
	}
	isNew, err := s.db.TryDowntimeEventDedup(station, d.EdgeEventID)
	if err != nil {
		log.Printf("core_handler: downtime event dedup station=%s edge_id=%d: %v", station, d.EdgeEventID, err)
		return
	}
	if !isNew {
		s.resp.dbg("downtime event replay station=%s edge_id=%d — already projected", station, d.EdgeEventID)
		return
	}
	ev := downtime.DowntimeEvent{
		Station:     station,
		PLCName:     d.PLCName,
		Reason:      d.Reason,
		StartedAt:   d.StartedAt,
		EndedAt:     d.EndedAt,
		DurationMS:  d.DurationMS,
		EdgeEventID: d.EdgeEventID,
	}
	select {
	case s.downtimeCh <- ev:
	default:
		log.Printf("core_handler: downtime event projection queue full, dropped station=%s plc=%s edge_id=%d", station, d.PLCName, d.EdgeEventID)
	}
}
