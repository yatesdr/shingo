package messaging

import (
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/store"
)

// ProductionReporter accumulates production deltas by cat_id and periodically
// enqueues production.report messages via the outbox for reliable delivery.
type ProductionReporter struct {
	db        *store.DB
	stationID string
	interval  time.Duration

	mu          sync.Mutex
	accumulator map[string]float64 // cat_id -> count

	stopOnce sync.Once
	stopCh   chan struct{}

	DebugLog func(string, ...any)
}

func (pr *ProductionReporter) dbg(format string, args ...any) {
	if fn := pr.DebugLog; fn != nil {
		fn(format, args...)
	}
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

// RecordDelta adds a production delta for a given job style.
// It looks up the style's cat_id; if empty, the delta is silently ignored.
func (pr *ProductionReporter) RecordDelta(jobStyleID int64, delta int64) {
	if delta <= 0 {
		return
	}
	style, err := pr.db.GetJobStyle(jobStyleID)
	if err != nil || style == nil || len(style.CatIDs) == 0 {
		return
	}
	pr.mu.Lock()
	for _, catID := range style.CatIDs {
		pr.accumulator[catID] += float64(delta)
	}
	pr.mu.Unlock()
	pr.dbg("delta recorded: style=%d delta=%d cat_ids=%v", jobStyleID, delta, style.CatIDs)
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
	// Swap out the accumulator
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
		return
	}
	data, err := env.Encode()
	if err != nil {
		log.Printf("production_reporter: encode envelope: %v", err)
		return
	}
	if _, err := pr.db.EnqueueOutbox(data, protocol.SubjectProductionReport); err != nil {
		log.Printf("production_reporter: enqueue outbox: %v", err)
	} else {
		log.Printf("production_reporter: enqueued %d cat_id entries via outbox", len(entries))
		pr.dbg("flush: enqueued %d cat_id entries", len(entries))
	}
}
