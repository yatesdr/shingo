package messaging

import (
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
//     change (one full snapshot — a message per process).
//   - a periodic full snapshot (snapshotInterval), so a late-joining or
//     restarted Core rebuilds its mirror from the most recent snapshot.
//   - PublishAll on boot/reconnect (Start calls it once).
//
// Together the periodic + boot snapshots replace Kafka compaction for late
// joiners: Core persists the mirror on every message, and a snapshot rebuilds
// it from scratch. Loaders/unloaders (manual_swap claims) are EXCLUDED here —
// they enter the computation as pool supply/demand via the loader aggregate,
// never as style claims.
type PlantClaimsPublisher struct {
	db        *store.DB
	stationID string
	// snapshotInterval is the full-snapshot cadence. Defaults to 5 minutes —
	// long enough to avoid chatter, short enough that a late-joining Core is
	// current within a few minutes of reconnect. Mirrors the cadence shape of
	// other periodic Edge publishers.
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
		snapshotInterval: 5 * time.Minute,
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
// periodic tick. Publishes a full snapshot (one message per process).
func (p *PlantClaimsPublisher) PublishChanged() error {
	return p.PublishAll()
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
	for _, proc := range procs {
		if err := p.publishProcess(proc); err != nil {
			log.Printf("plant_claims: publish %s: %v", proc.Name, err)
		}
	}
	return nil
}

func (p *PlantClaimsPublisher) publishProcess(proc processes.Process) error {
	styles, err := processes.ListStylesByProcess(p.db.DB, proc.ID)
	if err != nil {
		return err
	}
	report := protocol.PlantClaimsReport{
		ProcessID: proc.Name,
		Styles:    make([]protocol.PlantClaimsStyle, 0, len(styles)),
	}
	for _, st := range styles {
		claims, err := processes.ListClaims(p.db.DB, st.ID)
		if err != nil {
			return err
		}
		wire := protocol.PlantClaimsStyle{StyleID: st.Name}
		for _, c := range claims {
			if c.SwapMode == protocol.SwapModeManualSwap {
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
	return p.enqueue(report)
}

func (p *PlantClaimsPublisher) enqueue(report protocol.PlantClaimsReport) error {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectPlantClaims,
		protocol.Address{Role: protocol.RoleEdge, Station: p.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&report,
	)
	if err != nil {
		return err
	}
	data, err := env.Encode()
	if err != nil {
		return err
	}
	if _, err := p.db.EnqueueOutbox(data, protocol.SubjectPlantClaims); err != nil {
		return err
	}
	p.DebugLog.Log("published %s: %d styles", report.ProcessID, len(report.Styles))
	return nil
}
