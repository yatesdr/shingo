package engine

import (
	"context"
	"time"
)

// R1 shadow read-model.
//
// The threshold monitor decides replenishment off Core's ledger
// (SystemUOPForPayload). When a bin strands `staged` at the line without
// binding, the ledger keeps its delivered count while Edge's own counters drain
// to the truth — the SNF3 CARRIER-0024 shape (Core 150 vs the tile at 46), and
// the ledger silently suppresses ordering while the line starves. R1 has Edge
// report its per-consuming-node lineside levels every 60s to a Core read-model
// table (edge_lineside_reports, v52); Core computes the monitor's lineside term
// BOTH ways and logs any disagreement that would change a firing decision.
//
// It is SHADOW: Core still decides off the ledger. Nothing here changes what the
// monitor fires. The flip to deciding off the Edge reports is a one-line switch
// (swap edgeAdjustedTotal for ledgerTotal in the fire gate) once the shadow log
// shows agreement — deliberately NOT taken here.

// linesideReportStaleness is the R1 staleness window. An Edge lineside report
// older than this is not trusted for the shadow comparison — the monitor falls
// back to the ledger for that node (no adjustment) and flags the stale report.
const linesideReportStaleness = 3 * time.Minute

// ShadowCompareLineside runs the R1 shadow for each payload that appeared in a
// just-arrived Edge lineside report. Called by the messaging layer after the
// report rows are upserted. Reads-only; decides nothing.
func (m *ThresholdMonitor) ShadowCompareLineside(payloadCodes []string) {
	seen := make(map[string]struct{}, len(payloadCodes))
	for _, p := range payloadCodes {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		m.shadowCompareForPayload(p)
	}
}

func (m *ThresholdMonitor) shadowCompareForPayload(payload string) {
	if m.eng == nil || m.eng.db == nil || m.eng.inventoryService == nil {
		return
	}
	m.mu.Lock()
	bindings, monitored := m.thresholdsByPayload[payload]
	m.mu.Unlock()
	if !monitored || len(bindings) == 0 {
		return
	}

	reports, err := m.eng.db.ListLinesideReportsForPayload(payload)
	if err != nil {
		m.eng.logFn("threshold_monitor: R1 shadow list reports for %s: %v", payload, err)
		return
	}
	if len(reports) == 0 {
		return
	}

	ledgerTotal, err := m.readTotal(context.Background(), payload)
	if err != nil {
		m.eng.logFn("threshold_monitor: R1 shadow readTotal(%s): %v", payload, err)
		return
	}
	ledgerByNode, err := m.eng.inventoryService.LinesideLedgerByNode(context.Background(), payload)
	if err != nil {
		m.eng.logFn("threshold_monitor: R1 shadow ledger-by-node(%s): %v", payload, err)
		return
	}

	// Adjustment = sum over FRESH reported nodes of (edge view − ledger view).
	// Stale nodes fall back to the ledger (no adjustment) and are flagged.
	now := time.Now()
	adjustment := 0
	freshNodes := 0
	for _, r := range reports {
		age := now.Sub(r.ReportedAt)
		if age >= linesideReportStaleness {
			m.eng.logFn("threshold_monitor: R1 shadow STALE report — station=%s node=%s payload=%s reported %s ago (>= %s); falling back to the ledger for this node",
				r.Station, r.CoreNodeName, payload, age.Round(time.Second), linesideReportStaleness)
			continue
		}
		edgeNode := r.BinUOP + r.BucketQty
		ledgerNode := ledgerByNode[r.CoreNodeName]
		adjustment += edgeNode - ledgerNode
		freshNodes++
	}
	if freshNodes == 0 {
		return
	}
	edgeAdjustedTotal := ledgerTotal + adjustment

	for _, b := range bindings {
		if b.threshold <= 0 {
			continue
		}
		ledgerBelow := ledgerTotal < b.threshold
		edgeBelow := edgeAdjustedTotal < b.threshold
		if ledgerBelow != edgeBelow {
			m.eng.logFn("threshold_monitor: R1 shadow DISAGREEMENT — payload=%s loader=%s threshold=%d: ledger total=%d would %s, edge-adjusted total=%d would %s (adjustment=%+d over %d fresh node(s)). DECIDING OFF THE LEDGER (shadow).",
				payload, b.coreNodeName, b.threshold,
				ledgerTotal, fireVerb(ledgerBelow),
				edgeAdjustedTotal, fireVerb(edgeBelow),
				adjustment, freshNodes)
		}
	}
}

func fireVerb(below bool) string {
	if below {
		return "FIRE"
	}
	return "hold"
}
