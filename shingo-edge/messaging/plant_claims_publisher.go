package messaging

import (
	"fmt"
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// PlantClaimsPublisher publishes Edge's plant-spec claim set to Core on the
// plant.claims subject (Edge → Core), so Core's sourceability computation can
// mirror what every process can source and change over to without depending
// on Edge being up.
//
// Edge stays the source of truth for the plant spec; this publisher plumbs it
// onto Core. Three publish triggers:
//   - PublishChanged: called by the spec-edit handlers on every style/claim
//     change (one message, for the process the edit touched).
//   - PublishAll on registration — Start calls it at boot, and the
//     SubjectEdgeRegistered handler calls it again on every re-register, which
//     covers Core restarting (Core sends EdgeRegisterRequest to an edge it does
//     not know, the edge re-registers, and the ack lands on that handler).
//   - a periodic full snapshot (snapshotInterval) as the last-resort safety net
//     for a change whose publish was lost outright.
//
// Together the periodic + boot snapshots replace Kafka compaction for late
// joiners: Core persists the mirror on every message, and a snapshot rebuilds
// it from scratch. Loaders/unloaders (manual_swap claims) are EXCLUDED here —
// they enter the computation as pool supply/demand via the loader aggregate,
// never as style claims.
type PlantClaimsPublisher struct {
	db        *store.DB
	stationID string
	// snapshotInterval is the full-snapshot cadence — the SAFETY NET, not the
	// delivery mechanism. Changes are published by PublishChanged, and a
	// register/re-register publishes a full snapshot, so this only has to catch
	// a change whose publish was lost entirely.
	//
	// It was 5 minutes, which cost ~65 messages an hour at Springfield (one per
	// process, twelve times an hour) for config that changes a few times a
	// shift. That made plant.claims 66% of all envelopes Core discarded for
	// expiry — 181 a day — and every one of them was carrying config identical
	// to the snapshot before it.
	snapshotInterval time.Duration

	stopOnce sync.Once
	stopCh   chan struct{}

	DebugLog DebugLogFunc
}

// NewPlantClaimsPublisher creates a publisher for the given edge identity.
// Start begins the periodic snapshot loop and publishes one full snapshot
// immediately.
func NewPlantClaimsPublisher(db *store.DB, stationID string) *PlantClaimsPublisher {
	return &PlantClaimsPublisher{
		db:               db,
		stationID:        stationID,
		snapshotInterval: 60 * time.Minute,
		stopCh:           make(chan struct{}),
	}
}

// Start publishes one full snapshot immediately (so a freshly-Booted Core
// gets the current spec without waiting for the first tick), then begins the
// periodic snapshot loop.
func (p *PlantClaimsPublisher) Start() {
	if err := p.PublishAll(); err != nil {
		log.Printf("plant_claims: initial publish: %v", err)
	}
	go p.loop()
}

// Stop halts the periodic loop.
func (p *PlantClaimsPublisher) Stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
}

func (p *PlantClaimsPublisher) loop() {
	ticker := time.NewTicker(p.snapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if err := p.PublishAll(); err != nil {
				log.Printf("plant_claims: periodic publish: %v", err)
			}
		}
	}
}

// PublishChanged is the spec-change hook: the edit handlers call it after any
// style or claim mutation so Core sees the new spec without waiting for the
// periodic tick. It publishes ONE process's report — the one the edit
// touched. A PlantClaimsReport is complete for its process and Core replaces
// its mirror per ProcessID on receipt (HandlePlantClaims →
// plantclaims.ReplaceProcess deletes WHERE process_id = $1), so the other
// processes' mirrors are untouched by construction. It used to publish the
// whole plant, which was ~(1 + processes + Σ styles) queries per claim save.
//
// It goes through plain Enqueue, NOT EnqueueSnapshot. EnqueueSnapshot deletes
// every unsent plant.claims row before inserting, on the argument that each
// message is a complete snapshot — of the PLANT. A single-process message is
// a complete snapshot of one process, so superseding through it would delete
// a pending full snapshot's other processes. The volume EnqueueSnapshot was
// added against does not come back this way: that was the 5-minute timer
// (12 full snapshots an hour, ~65 messages) accumulating through an outage,
// and spec edits are a few per shift — a whole outage holds a handful of
// one-process rows, each tiny, and the hourly full snapshot (still on
// EnqueueSnapshot) supersedes them anyway. Ordering is by outbox id, so a
// per-process row enqueued after a pending full snapshot is delivered after
// it and wins at Core.
func (p *PlantClaimsPublisher) PublishChanged(processID int64) error {
	proc, err := processes.Get(p.db.DB, processID)
	if err != nil {
		return fmt.Errorf("plant_claims: process %d: %w", processID, err)
	}
	data, err := p.buildProcess(*proc)
	if err != nil {
		return fmt.Errorf("plant_claims: build %s: %w", proc.Name, err)
	}
	if _, err := p.db.EnqueueOutbox(data, protocol.SubjectPlantClaims); err != nil {
		return fmt.Errorf("plant_claims: enqueue %s: %w", proc.Name, err)
	}
	return nil
}

// PublishAll reads the current plant spec and publishes one PlantClaimsReport
// per process. A process with zero sourceability-relevant claims still gets a
// report (empty styles) so Core drops its stale mirror. Returns an error only
// if the spec read itself fails; per-process publish errors are logged and
// skipped so one bad process can't block the rest.
func (p *PlantClaimsPublisher) PublishAll() error {
	procs, err := processes.List(p.db.DB)
	if err != nil {
		return err
	}
	// Build every process's payload first, then enqueue the whole set in ONE
	// call. EnqueueSnapshot supersedes the unsent predecessors, so this has to
	// be atomic across processes: enqueuing per-process would have each one
	// delete the payloads the previous ones just wrote, and Core would end up
	// mirroring a single process.
	payloads := make([][]byte, 0, len(procs))
	for _, proc := range procs {
		data, err := p.buildProcess(proc)
		if err != nil {
			// One unreadable process must not block the rest — the same
			// contract this loop had when it published per process.
			log.Printf("plant_claims: build %s: %v", proc.Name, err)
			continue
		}
		payloads = append(payloads, data)
	}
	if len(payloads) == 0 {
		return nil
	}
	return p.db.EnqueueSnapshotOutbox(payloads, protocol.SubjectPlantClaims)
}

// buildProcess reads one process's spec in TWO queries — its live styles and
// all of their claims — and groups in Go. It was one ListClaims per style
// inside the loop, ~(1 + styles) queries on a store pinned to a single
// connection, and every spec edit paid it for every process at the plant.
// Grouping by StyleID preserves what the per-style read gave: the claims
// arrive ordered by (style_id, sequence, core_node_name), so each style's
// slice is in ListClaims order and Core's Seq column does not churn.
func (p *PlantClaimsPublisher) buildProcess(proc processes.Process) ([]byte, error) {
	styles, err := processes.ListStylesByProcess(p.db.DB, proc.ID)
	if err != nil {
		return nil, err
	}
	allClaims, err := processes.ListLiveClaimsByProcess(p.db.DB, proc.ID)
	if err != nil {
		return nil, err
	}
	claimsByStyle := make(map[int64][]processes.NodeClaim, len(styles))
	for _, c := range allClaims {
		claimsByStyle[c.StyleID] = append(claimsByStyle[c.StyleID], c)
	}
	report := protocol.PlantClaimsReport{
		ProcessID: proc.Name,
		Styles:    make([]protocol.PlantClaimsStyle, 0, len(styles)),
	}
	for _, st := range styles {
		// Mark the running style. proc.ActiveStyleID is the field Edge itself
		// resolves claims through (requestedClaimAtNode keys on it), so publishing it
		// tells Core what Edge is already acting on rather than a second,
		// separately-maintained notion of "running".
		wire := protocol.PlantClaimsStyle{
			StyleID: st.Name,
			Active:  proc.ActiveStyleID != nil && *proc.ActiveStyleID == st.ID,
		}
		for _, c := range claimsByStyle[st.ID] {
			if c.IsLoaderNode() {
				continue // loaders/unloaders excluded — pool, not claims
			}
			wire.Claims = append(wire.Claims, protocol.PlantClaim{
				CoreNodeName:        c.CoreNodeName,
				Role:                c.Role,
				SwapMode:            c.SwapMode,
				PayloadCode:         c.PayloadCode,
				AllowedPayloadCodes: c.AllowedPayloads(),
				UOPCapacity:         c.UOPCapacity,
				ReorderPoint:        c.ReorderPoint,
			})
		}
		report.Styles = append(report.Styles, wire)
	}
	return p.encode(report)
}

// encode builds one process's envelope. It does NOT touch the outbox — the
// caller enqueues the whole set together, because a per-process enqueue would
// make each process supersede the last.
func (p *PlantClaimsPublisher) encode(report protocol.PlantClaimsReport) ([]byte, error) {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectPlantClaims,
		protocol.Address{Role: protocol.RoleEdge, Station: p.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&report,
	)
	if err != nil {
		return nil, err
	}
	data, err := env.Encode()
	if err != nil {
		return nil, err
	}
	p.DebugLog.Log("built %s: %d styles", report.ProcessID, len(report.Styles))
	return data, nil
}
