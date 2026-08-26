package engine

import (
	"log"
	"time"

	"shingo/protocol"
)

// R1 lineside reporter (Edge side).
//
// Edge is authoritative for lineside counts after the bin-ownership flip. When
// a bin strands `staged` at the line without binding, Core's own ledger keeps
// the delivered count and reads STOCKED while Edge's counters drain to the
// truth (the SNF3 CARRIER-0024 shape: Core 150 vs the tile at 46), so Core
// silently suppresses ordering while the line starves. This reporter pushes
// Edge's per-consuming-node lineside on-hand to Core every 60s so Core can
// correct for that.
//
// ⚠️ NOT reporting-only, and it has not been since c20cf5aa (2026-07-24). Under
// the default lineside_decision_mode=edge_reports these reports DECIDE
// replenishment on Core; lineside_decision_mode=ledger reverts to the pure
// ledger, which is config rather than code. A report that does not arrive is
// not a missed log line — Core's staleness window is 3 minutes, and a node with
// no fresh report falls back to the ledger term for the interval.

// linesideReportInterval is the R1 reporter cadence. Core trusts a report for
// linesideReportStaleness (3 minutes), so this is deliberate 3x margin: two
// consecutive reports can be lost before a node drops out of the adjustment.
const linesideReportInterval = 60 * time.Second

// startLinesideReporter spawns the R1 lineside reporter goroutine.
func (e *Engine) startLinesideReporter() {
	go e.runLinesideReporter()
}

func (e *Engine) runLinesideReporter() {
	ticker := time.NewTicker(linesideReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			e.reportLinesideLevels()
		}
	}
}

func (e *Engine) reportLinesideLevels() {
	levels, err := e.db.ListLinesideLevels()
	if err != nil {
		log.Printf("lineside-reporter: list levels: %v", err)
		return
	}
	if len(levels) == 0 {
		return
	}

	station := e.cfg.StationID()
	entries := make([]protocol.LinesideLevelEntry, 0, len(levels))
	for _, l := range levels {
		entries = append(entries, protocol.LinesideLevelEntry{
			CoreNodeName: l.CoreNodeName,
			PayloadCode:  l.PayloadCode,
			BinCount:     l.BinCount,
			BinUOP:       l.BinUOP,
			BucketQty:    l.BucketQty,
		})
	}

	env, err := protocol.NewDataEnvelope(
		protocol.SubjectLinesideLevelReport,
		protocol.Address{Role: protocol.RoleEdge, Station: station},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.LinesideLevelReport{
			Station:    station,
			ReportedAt: time.Now().UTC(),
			Entries:    entries,
		},
	)
	if err != nil {
		log.Printf("lineside-reporter: build envelope: %v", err)
		return
	}
	data, err := env.Encode()
	if err != nil {
		log.Printf("lineside-reporter: encode envelope: %v", err)
		return
	}
	// Snapshot enqueue: this report carries EVERY consuming node, so an unsent
	// predecessor is worthless. Without this, an outage leaves an hour of
	// superseded reports to publish in a burst on recovery — and Core discards
	// most of them for expiry on arrival anyway.
	if err := e.db.EnqueueSnapshotOutbox([][]byte{data}, protocol.SubjectLinesideLevelReport); err != nil {
		log.Printf("lineside-reporter: enqueue: %v", err)
	}
}
