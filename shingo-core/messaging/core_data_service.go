package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/dispatch"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/downtime"
	"shingocore/store/heartbeat"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
	"shingocore/store/registry"
)

// heartbeatRetentionDays is the cell_part_events retention window (plan §12).
const heartbeatRetentionDays = 90

// downtimeRetentionDays matches heartbeatRetentionDays. Same event-log shape,
// same 90-day window; no reason for the two to differ.
const downtimeRetentionDays = 90

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
	// OnLinesideReports is the R1 report-arrival trigger for the payloads in a
	// just-arrived Edge report. R1 is LIVE: in edge_reports mode a fresh report is
	// a fire trigger (decide off the edge-adjusted total); in ledger mode it stays
	// audit-only. Either way it logs the ledger-vs-edge disagreement audit line.
	OnLinesideReports(payloadCodes []string)
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
func NewCoreDataService(db *store.DB, resp coreDataResponder, announce service.EpochAnnounce) *CoreDataService {
	return &CoreDataService{
		db:             db,
		tagVerify:      service.NewTagVerifyService(db),
		inventoryDelta: service.NewInventoryDeltaService(db, service.NewBinManifestService(db, announce), announce),
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
			// The dedup guard rides the same ticker and the same window as
			// the projection it guards. It grew at exactly the rate of
			// cell_part_events — same event, same function, identical row
			// count — while only one of the two was bounded, which at 40
			// cells is 2.2 MB/day forever. It cannot be partitioned (see
			// heartbeat.PurgeOldDedup), so it is a DELETE.
			if purged, err := s.db.PurgeOldProductionTickDedup(heartbeatRetentionDays, now); err != nil {
				log.Printf("core_handler: purge old production tick dedup: %v", err)
			} else if purged > 0 {
				log.Printf("core_handler: purged %d expired production.tick dedup row(s)", purged)
			}
			// The bin_uop_delta_daily roll-up (v94) rides the same daily
			// ticker as the purges — it must run while the raw delta rows
			// are still inside the 90-day window, and the one-day-ago day
			// boundary is the natural cadence: yesterday is complete, still
			// raw-resident, and re-derivable if this attempt fails (the
			// upsert is idempotent per day).
			if rolled, err := s.db.RollupBinUOPDeltaDay(now.AddDate(0, 0, -1)); err != nil {
				log.Printf("core_handler: rollup bin_uop_delta_daily: %v", err)
			} else if rolled > 0 {
				log.Printf("core_handler: rolled up %d bin_uop_delta_daily row(s) for %s",
					rolled, now.AddDate(0, 0, -1).UTC().Format("2006-01-02"))
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
// THE STATION COMES FROM THE ENVELOPE, FULL STOP. It used to come from the
// payload with the envelope as a fallback, which is a rule with two possible
// answers that only ever produced one because a plant had exactly one station.
// Per-edge identity is the change that makes them able to disagree, so the
// payload copy is gone and this reads the source the transport carried.
func (s *CoreDataService) HandleProductionTick(env *protocol.Envelope, snap *protocol.CounterSnapshot) {
	station := env.Src.Station
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
	// correct, dedup-guarded placement for the IncrementProduced
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
	// The station is the ENVELOPE's, which is the one the transport carried
	// rather than the one the sender asserted in its own body. See
	// HandleProductionTick.
	station := env.Src.Station
	if err := s.inventoryDelta.ApplyBinUOPDelta(station, d); err != nil {
		if errors.Is(err, service.ErrInventoryDeltaSkipped) {
			s.resp.dbg("bin_uop_delta replay station=%s bin=%d seq=%d — already applied",
				station, d.BinID, d.SequenceID)
			return
		}
		log.Printf("core_handler: apply BinUOPDelta station=%s bin=%d seq=%d delta=%d reason=%s: %v",
			station, d.BinID, d.SequenceID, d.Delta, d.Reason, err)
		return
	}
	s.resp.dbg("bin_uop_delta applied station=%s bin=%d seq=%d delta=%d reason=%s",
		station, d.BinID, d.SequenceID, d.Delta, d.Reason)

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
			d.PayloadCode, station, qty, d.Reason)
		if err := s.db.IncrementProduced(d.PayloadCode, qty); err != nil {
			log.Printf("core_handler: increment produced payload=%s qty=%d: %v", d.PayloadCode, qty, err)
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
	station := env.Src.Station
	if err := s.inventoryDelta.ApplyLinesideBucketDelta(station, d); err != nil {
		if errors.Is(err, service.ErrInventoryDeltaSkipped) {
			s.resp.dbg("lineside_bucket_delta replay station=%s core_node=%q part=%q seq=%d — already applied",
				station, d.CoreNodeName, d.PartNumber, d.SequenceID)
			return
		}
		log.Printf("core_handler: apply LinesideBucketDelta station=%s core_node=%q part=%q seq=%d delta=%d reason=%s: %v",
			station, d.CoreNodeName, d.PartNumber, d.SequenceID, d.Delta, d.Reason, err)
		return
	}
	s.resp.dbg("lineside_bucket_delta applied station=%s core_node=%q part=%q seq=%d delta=%d reason=%s",
		station, d.CoreNodeName, d.PartNumber, d.SequenceID, d.Delta, d.Reason)

	// Notify the UOP-threshold monitor so a bucket drain or capture
	// re-evaluates loop totals. The monitor's debounce + opt-in gating
	// inside is what keeps this from being noisy. Empty payload_code is
	// fine — the monitor short-circuits on unknown payload.
	if s.thresholdMonitor != nil {
		s.thresholdMonitor.OnBucketApplied(station, d.CoreNodeName, d.PayloadCode, d.Delta, d.Reason)
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
	uid := p.StationID
	log.Printf("core_handler: edge registered: uid=%s (hostname=%s, instance=%s, version=%s)",
		uid, p.Hostname, p.Instance, p.Version)

	conflict, err := s.db.RegisterEdge(uid, p.Hostname, p.Instance, p.Version)
	if errors.Is(err, registry.ErrUnknownStation) {
		// AN EDGE MAY INTRODUCE ITSELF. IT MAY NOT SAY WHICH STATION IT IS.
		//
		// This is the branch the enrollment deploy deleted, rebuilt on the
		// distinction the first version missed: the defect was COLLISION, not
		// CREATION. The old station id was composed from two struct defaults,
		// so every unconfigured Pi in the fleet asserted the SAME string —
		// they did not collide, they took turns owning one row. An edge that
		// mints 64 random bits cannot reach another station's row at all. It
		// can only make its own.
		//
		// So refusing creation bought nothing that randomness does not buy
		// structurally, and it cost the thing that made an edge deployable:
		// coming up at all without a human first fetching a value from another
		// system. That put a distributed-identity concept on the shop floor.
		//
		// What is NOT restored is the deleted branch's actual sin. That one
		// called Enroll and produced a row indistinguishable from a station
		// somebody had deliberately created. This one leaves claimed_at NULL
		// and display_name empty, so the station is visibly unacknowledged
		// everywhere it is listed until a human says what it is. It runs
		// meanwhile — its work is attributed to its own uid and to nothing
		// else, which is the property that makes running-while-unclaimed safe.
		if _, ierr := s.db.IntroduceEdge(uid, p.Hostname, p.Version); ierr != nil &&
			!errors.Is(ierr, registry.ErrAlreadyEnrolled) {
			log.Printf("core_handler: introduce unknown station %s: %v", uid, ierr)
			return
		}
		// Re-register so the binding lease, instance and conflict detection all
		// run against the row exactly as they would for any other station.
		conflict, err = s.db.RegisterEdge(uid, p.Hostname, p.Instance, p.Version)
	}
	if err != nil {
		log.Printf("core_handler: register edge %s: %v", uid, err)
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

	// THE ACK CARRIES THE CONFLICT, so the warning appears in the EDGE's journal
	// as well as Core's. On the floor you are stood in front of a Pi, not in
	// front of Core, and main.go's edge.registered handler already prints
	// `msg=%s` — so this costs nothing on the Edge side and puts the sentence
	// where the person diagnosing it is looking.
	//
	// It reaches whichever edge holds the topic partition, which with two
	// duplicate edges is not necessarily the one that registered — that is
	// exactly the single-partition consumer-group defect, and it is why Core's
	// journal and the persisted conflict_* columns are the primary record and
	// this is the extra one.
	msg := "registered"
	if conflict != nil {
		msg = "registered, BUT " + conflict.String() +
			" — enroll the second edge as its own station on Core and put ITS station_uid in that Pi's shingoedge.yaml"
	}
	s.resp.replyData(env, protocol.SubjectEdgeRegistered,
		&protocol.EdgeRegistered{StationID: p.StationID, Message: msg})
	s.resp.dbg("reply published: subject=edge.registered station=%s", p.StationID)

	// Derive demand_registry for this station from the Core-owned loader aggregate
	// on (re)connect, so a plant configured entirely through the UI gets live
	// demand routing without an out-of-band seeddev/migrateloaders run. Idempotent
	// — SyncDemandRegistry diffs against the current rows. (The Edge pushes no
	// claim config over the wire; the aggregate is the sole source.)
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

// HandleEdgeHeartbeat marks an enrolled station alive.
//
// THE HEARTBEAT NO LONGER CREATES ROWS, and that is half of guard 2 rather
// than a tidy-up. Refusing at Register alone would have been theatre: the old
// UpdateHeartbeat upserted and set status='active', so an unknown machine's
// row appeared sixty seconds later anyway — with no hostname and no version on
// it, which is strictly worse evidence than the register would have left.
// found=false drives the same edge.register_request the old isNew flag did;
// the difference is that the request is now the only outcome.
func (s *CoreDataService) HandleEdgeHeartbeat(env *protocol.Envelope, p *protocol.EdgeHeartbeat) {
	found, err := s.db.UpdateHeartbeat(p.StationID)
	if err != nil {
		log.Printf("core_handler: update heartbeat for %s: %v", p.StationID, err)
		return
	}

	s.resp.replyData(env, protocol.SubjectEdgeHeartbeatAck,
		&protocol.EdgeHeartbeatAck{StationID: p.StationID, ServerTS: clock.Now().UTC()})

	if !found {
		log.Printf("core_handler: heartbeat from unenrolled station %s, requesting registration", p.StationID)
		s.resp.sendData(protocol.SubjectEdgeRegisterRequest, p.StationID,
			&protocol.EdgeRegisterRequest{StationID: p.StationID, Reason: "station not enrolled"})
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

	var infos []protocol.NodeInfo
	if stationScoped {
		for _, n := range nodeList {
			name := n.Name
			if n.ParentID != nil && !n.IsSynthetic && n.ParentName != "" {
				name = n.ParentName + "." + n.Name
			}
			infos = append(infos, protocol.NodeInfo{
				Name:     name,
				NodeType: n.NodeTypeCode,
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
						Name:     parent.Name + "." + n.Name,
						NodeType: n.NodeTypeCode,
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
		// Sending the node list WITHOUT loaders is not "degraded but safe" — the Edge
		// cannot distinguish an absent Loaders field from "no loaders configured", and
		// ReplaceCoreLoaders(nil) truncates all five cache tables. Send nothing; the
		// Edge keeps its last-known-good cache and re-requests on the next tick.
		log.Printf("core_handler: build loader infos for %s: %v — node list NOT sent", env.Src.Station, lerr)
		return
	}
	// Payload→dunnage mapping: one query replaces the N+1 per-node
	// GetEffectiveBinTypes calls. Edge uses this to derive picker options
	// from the node's allowed payloads (claim.AllowedPayloadCodes).
	//
	// Unlike the loader branch above, a read failure here deliberately does
	// NOT return: this slice is memory-only on the Edge (re-derived from the
	// next node list), while the loader slice backs a durable cache that a
	// wrong read destroys. Do not "unify" the two branches.
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
		// SOLE writer of produced_qty. IncrementProduced is
		// NOT idempotent, so we must NOT also write here — double-writing would
		// silently double the counter and the parity check would pass on both
		// being wrong. Keep the handler + ack live and LOG what this path WOULD
		// have written so Stephen can compare LOGS (not counter values) for a
		// week before the production_reporter deletion lands (Q-024-FOLLOWUP).
		// (The production_log half of the old claim was a duplicate ledger,
		// dropped at v92 — this path's counter claim is the surviving half.)
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
	catids, err := s.db.PayloadCATIDs()
	if err != nil {
		// Degrade to no CATIDs rather than failing the whole catalog sync — the
		// edge just won't auto-fill expected_catid this round.
		log.Printf("core_handler: payload catids for catalog: %v", err)
		catids = map[int64]string{}
	}
	infos := make([]protocol.CatalogPayloadInfo, len(payloads))
	for i, p := range payloads {
		infos[i] = protocol.CatalogPayloadInfo{
			ID: p.ID, Name: p.Code, Code: p.Code,
			Description: p.Description,
			UOPCapacity: p.UOPCapacity,
			CATID:       catids[p.ID],
		}
	}
	s.resp.replyData(env, protocol.SubjectCatalogPayloadsResponse, &protocol.CatalogPayloadsResponse{Payloads: infos})
	log.Printf("core_handler: sent payload catalog (%d payloads) to %s", len(infos), env.Src.Station)
}

// HandleOrderStatusRequest answers an Edge's reconcile.
//
// TWO HALVES, and the second is the one that matters for Core-authored orders.
// The first is the original: a snapshot per UUID the Edge named, so it can
// correct any status it has stale. The second is new — every non-terminal order
// Core holds for that station which the Edge did NOT name — so an order the Edge
// has no row for gets one.
//
// That half is load-bearing rather than a backstop. The Core → Edge outbox drops
// a message permanently once it exhausts its retries, so an order projection
// that never lands is an ordinary event, and this is the only thing that repairs
// it. An Edge running against an older Core simply gets the first half, which is
// where it is today.
func (s *CoreDataService) HandleOrderStatusRequest(env *protocol.Envelope, req *protocol.OrderStatusRequest) {
	resp := &protocol.OrderStatusResponse{Orders: make([]protocol.OrderStatusSnapshot, 0, len(req.OrderUUIDs))}
	asked := make(map[string]bool, len(req.OrderUUIDs))
	for _, orderUUID := range req.OrderUUIDs {
		asked[orderUUID] = true
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
	resp.Unlisted = s.unlistedFor(env.Src.Station, asked)
	s.resp.replyData(env, protocol.SubjectOrderStatusResponse, resp)
}

// unlistedFor collects this station's active orders that the Edge did not name.
//
// Scoped to the ASKING station, from the envelope rather than from anything in
// the request body: an Edge may only be healed with its own orders, and the
// envelope is the one statement of who is asking that the sender cannot restate
// incorrectly.
//
// A read failure returns nothing rather than failing the whole reply. The
// snapshots the Edge asked for are worth delivering on their own, and the heal
// retries on the next reconcile — dropping both halves because the second could
// not be built would turn a partial answer into no answer.
func (s *CoreDataService) unlistedFor(stationID string, asked map[string]bool) []protocol.OrderProjection {
	if stationID == "" {
		return nil
	}
	active, err := s.db.ListActiveOrdersByStation(stationID)
	if err != nil {
		log.Printf("core_handler: order reconcile for %s: listing active orders: %v — replying with snapshots only", stationID, err)
		return nil
	}
	var out []protocol.OrderProjection
	for _, o := range active {
		if o == nil || o.EdgeUUID == "" || asked[o.EdgeUUID] {
			continue
		}
		out = append(out, dispatch.ProjectionFor(o))
	}
	if len(out) > 0 {
		log.Printf("core_handler: order reconcile for %s: %d order(s) it has no row for — sending them down", stationID, len(out))
	}
	return out
}

// HandlePlantClaims mirrors a plant-claims report (Edge → Core) for one
// process into process_styles/style_claims and rebuilds the dirty index for
// that process. The message is authoritative for its process: the handler
// replaces the process's rows wholesale on every message, so a periodic full
// snapshot rebuilds late joiners (no Kafka compaction). Loaders/unloaders are
// already excluded by the publisher (manual_swap claims never appear); nothing
// here filters by swap_mode.
//
// ConfigGen is a stale-snapshot guard: if the mirror already holds a NEWER
// config_gen for this process (an out-of-order older snapshot landing after a
// newer one), the replace is a no-op. Zero ConfigGen means "not tracked" and
// is always applied.
func (s *CoreDataService) HandlePlantClaims(env *protocol.Envelope, report *protocol.PlantClaimsReport) {
	if report.ProcessID == "" {
		log.Printf("core_handler: plant.claims from %s: empty process_id — ignored", env.Src.Station)
		return
	}

	var styles []plantclaims.StyleRow
	var claims []plantclaims.ClaimRow
	for _, st := range report.Styles {
		styles = append(styles, plantclaims.StyleRow{
			ProcessID: report.ProcessID,
			StyleID:   st.StyleID,
			ConfigGen: report.ConfigGen,
			IsActive:  st.Active,
		})
		for i, c := range st.Claims {
			claims = append(claims, plantclaims.ClaimRow{
				ProcessID:           report.ProcessID,
				StyleID:             st.StyleID,
				CoreNodeName:        c.CoreNodeName,
				Role:                c.Role,
				SwapMode:            c.SwapMode,
				PayloadCode:         c.PayloadCode,
				AllowedPayloadCodes: c.AllowedPayloadCodes,
				UOPCapacity:         c.UOPCapacity,
				ReorderPoint:        c.ReorderPoint,
				Seq:                 i,
			})
		}
	}

	if err := s.db.ReplacePlantClaims(report.ProcessID, styles, claims, report.ConfigGen); err != nil {
		log.Printf("core_handler: plant.claims mirror %s: %v", report.ProcessID, err)
		return
	}
	log.Printf("core_handler: plant.claims mirrored %s: %d styles, %d claims (config_gen=%d)",
		report.ProcessID, len(styles), len(claims), report.ConfigGen)
}

// StartDowntimeProjection launches the async downtime_events projection worker
// and partition manager (G9). Call once at the composition root after subject
// registration. Mirrors StartHeartbeatProjection: HandleDowntimeEvent enqueues,
// this worker does the INSERT.
//
// downtime_events is scaffolding for the sim's downtime model — deliberate,
// and not currently fed by any plant (0 rows at Springfield; the only producer
// is behind //go:build sim). It is kept and maintained, not deleted, because
// the sim work that consumes it is planned. What it must NOT do meanwhile is
// accumulate one empty partition a month forever, which is what happened while
// this loop was missing.
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
	// Daily maintenance, mirroring StartHeartbeatProjection. Two things were
	// missing here, not one: EnsurePartitions only creates the current and next
	// month, so with no daily tick a Core up longer than two months runs out of
	// partitions; and the copy that became store/downtime dropped
	// DropOldPartitions entirely, so nothing ever pruned.
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for range t.C {
			now := clock.Now().UTC()
			if err := s.db.EnsureDowntimePartitions(now); err != nil {
				log.Printf("core_handler: ensure downtime partitions: %v", err)
			}
			if dropped, err := s.db.DropOldDowntimePartitions(downtimeRetentionDays, now); err != nil {
				log.Printf("core_handler: drop old downtime partitions: %v", err)
			} else if dropped > 0 {
				log.Printf("core_handler: dropped %d expired downtime partition(s)", dropped)
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
	station := env.Src.Station
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
