package messaging

import (
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/store"
)

// ProductionReporter accumulates production deltas by payload code (cat_id)
// and periodically enqueues production.report messages via the outbox for
// reliable delivery.
type ProductionReporter struct {
	db        *store.DB
	stationID string
	interval  time.Duration

	mu          sync.Mutex
	accumulator map[string]float64 // payload code (cat_id) -> count

	stopOnce sync.Once
	stopCh   chan struct{}

	DebugLog DebugLogFunc
}

// NewProductionReporter creates a reporter for the given edge identity.
func NewProductionReporter(db *store.DB, stationID string) *ProductionReporter {
	return &ProductionReporter{
		db:          db,
		stationID:   stationID,
		interval:    60 * time.Second,
		accumulator: make(map[string]float64),
		stopCh:      make(chan struct{}),
	}
}

// RecordDelta adds a production delta for a produced part, keyed by its
// payload code — the catalog part code (cat_id) Core matches demands on.
// The payload is resolved at the produce-tick site and delivered via
// EventProducedReport, so reporting no longer depends on style naming.
func (pr *ProductionReporter) RecordDelta(payloadCode string, delta int64) {
	if delta <= 0 || payloadCode == "" {
		return
	}
	pr.mu.Lock()
	pr.accumulator[payloadCode] += float64(delta)
	pr.mu.Unlock()
	pr.DebugLog.Log("delta recorded: payload=%s delta=%d", payloadCode, delta)
}

// Start begins the periodic flush loop.
func (pr *ProductionReporter) Start() {
	go pr.loop()
}

// Stop flushes any remaining counts and halts the loop.
func (pr *ProductionReporter) Stop() {
	pr.stopOnce.Do(func() {
		close(pr.stopCh)
		pr.flush()
	})
}

func (pr *ProductionReporter) loop() {
	ticker := time.NewTicker(pr.interval)
	defer ticker.Stop()
	for {
		select {
		case <-pr.stopCh:
			return
		case <-ticker.C:
			pr.flush()
		}
	}
}

func (pr *ProductionReporter) flush() {
	pr.mu.Lock()
	if len(pr.accumulator) == 0 {
		pr.mu.Unlock()
		return
	}
	// Swap out the accumulator so new deltas don't block on this flush.
	snapshot := pr.accumulator
	pr.accumulator = make(map[string]float64)
	pr.mu.Unlock()

	var entries []protocol.ProductionReportEntry
	for catID, count := range snapshot {
		entries = append(entries, protocol.ProductionReportEntry{CatID: catID, Count: int64(count)})
	}

	env, err := protocol.NewDataEnvelope(
		protocol.SubjectProductionReport,
		protocol.Address{Role: protocol.RoleEdge, Station: pr.stationID},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.ProductionReport{
			StationID: pr.stationID,
			Reports:   entries,
		},
	)
	if err != nil {
		log.Printf("production_reporter: build envelope: %v", err)
		pr.restoreSnapshot(snapshot)
		return
	}
	data, err := env.Encode()
	if err != nil {
		log.Printf("production_reporter: encode envelope: %v", err)
		pr.restoreSnapshot(snapshot)
		return
	}
	if _, err := pr.db.EnqueueOutbox(data, protocol.SubjectProductionReport); err != nil {
		log.Printf("ERROR: production_reporter: enqueue outbox failed, restoring deltas: %v", err)
		pr.restoreSnapshot(snapshot)
	} else {
		pr.DebugLog.Log("flush: enqueued %d entries", len(entries))
	}
}

// restoreSnapshot merges a failed snapshot back into the accumulator so
// deltas are not lost when the outbox write fails.
func (pr *ProductionReporter) restoreSnapshot(snapshot map[string]float64) {
	pr.mu.Lock()
	for catID, count := range snapshot {
		pr.accumulator[catID] += count
	}
	pr.mu.Unlock()
}
