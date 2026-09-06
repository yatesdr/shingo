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
	var unknown []string
	for _, l := range levels {
		// A NODE WHOSE CARRIER NOBODY COULD IDENTIFY SHIPS NO ROW.
		//
		// There is no safe guess to make here. The old one — fall back to the
		// claim — is what put a part number of which zero existed plant-wide on
		// a carrier holding 7032 of something else, and because the payload is
		// the join key on Core it took the real part's correction down with it.
		//
		// Absence is the designed degradation and Core is already written for
		// it: past the 3-minute staleness window a node with no fresh report
		// falls back to its ledger term and makes no adjustment. That is a
		// less-corrected number, which is a different thing from a wrong one.
		if !l.PayloadKnown || l.PayloadCode == "" {
			unknown = append(unknown, l.CoreNodeName)
			continue
		}
		entries = append(entries, protocol.LinesideLevelEntry{
			CoreNodeName: l.CoreNodeName,
			PayloadCode:  l.PayloadCode,
			BinCount:     l.BinCount,
			BinUOP:       l.BinUOP,
			BucketQty:    l.BucketQty,
		})
	}
	// Loud, because a node dropping out of the adjustment is a real change in
	// what Core is deciding on and the operator-visible symptom is nothing at
	// all. A node that stays here across many reports has a carrier no delivery
	// envelope and no person has ever identified.
	if len(unknown) > 0 {
		log.Printf("lineside-reporter: %d node(s) withheld — no established carrier identity: %v",
			len(unknown), unknown)
	}
	if len(entries) == 0 {
		return
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
