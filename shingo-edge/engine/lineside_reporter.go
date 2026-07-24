package engine

import (
	"log"
	"time"

	"shingo/protocol"
)

// R1 shadow reporter (Edge side).
//
// Edge is authoritative for lineside counts after the bin-ownership flip, but
// Core's replenishment monitor still decides off its own ledger. When a bin
// strands `staged` at the line without binding, the two diverge and Core
// silently suppresses ordering (the SNF3 CARRIER-0024 shape: Core 150 vs the
// tile at 46). This reporter pushes Edge's own per-consuming-node lineside
// on-hand to Core every 60s so Core can shadow its lineside term against the
// truth and log firing-decision disagreements. Reporting only — no behavior
// change here, and Core decides off the ledger.

// linesideReportInterval is the R1 reporter cadence.
const linesideReportInterval = 60 * time.Second

// startLinesideReporter spawns the R1 shadow reporter goroutine.
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
	if _, err := e.db.EnqueueOutbox(data, protocol.SubjectLinesideLevelReport); err != nil {
		log.Printf("lineside-reporter: enqueue: %v", err)
	}
}
