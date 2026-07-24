package engine

import (
	"context"
	"time"
)

// R1 lineside read-model — LIVE.
//
// The threshold monitor decides replenishment off an in-loop UOP total. Core's
// ledger (SystemUOPForPayload) keeps a bin's delivered count even when that bin
// strands `staged` at the line without binding, so the ledger reads STOCKED while
// Edge's own counters drain to the truth — the SNF3 CARRIER-0024 shape (Core 150
// vs the tile at 46), and the ledger silently suppresses ordering while the line
// starves. R1 has Edge report its per-consuming-node lineside levels every 60s to
// a Core read-model table (edge_lineside_reports, v52).
//
// R1 is now LIVE (was SHADOW). By default (lineside_decision_mode=edge_reports)
// the fire gate decides off the Edge-report-adjusted total: the ledger total plus,
// for each FRESH reported node, (edge view − ledger view). A node whose latest
// report is missing OR stale (>= linesideReportStaleness) contributes NO
// adjustment — its ledger term stands — and is flagged. Setting
// lineside_decision_mode=ledger reverts to deciding off the pure ledger (the
// pre-R1 behavior — config, not code). Either way the ledger-vs-edge disagreement
// is logged permanently as the AUDIT TRAIL.
//
// This file owns the read-model math (linesideDecisionTotal), the audit log
// (auditLinesideDecision), and the report-arrival trigger (OnLinesideReports).
// It writes nothing to bins.uop_remaining — only edge_lineside_reports, its own
// table — and the delta path stays the only writer to the bins ledger.

// linesideReportStaleness is the R1 staleness window. An Edge lineside report
// older than this is not trusted for the adjustment — the monitor falls back to
// the ledger for that node (no adjustment) and flags the stale report.
const linesideReportStaleness = 3 * time.Minute

// linesideDecisionTotal computes, for one payload, the Edge-report-adjusted
// in-loop total (edgeAdjusted) alongside the pure ledger total (ledgerTotal), and
// whether fresh Edge reports actually moved the total (usedEdge).
//
// edgeAdjusted = ledgerTotal + Σ over FRESH reported nodes of (edge view − ledger
// view). A node whose latest report is missing OR older than linesideReportStaleness
// contributes no adjustment (its ledger term stands) and is flagged via the stale
// log. When no fresh report exists for the payload, edgeAdjusted == ledgerTotal and
// usedEdge is false.
//
// The ledger read error is propagated so the caller SKIPS the evaluation rather
// than firing off a zero (the same contract the old readTotal path had). A failure
// to read the reports table or the per-node ledger degrades SAFELY to the pure
// ledger (log + usedEdge=false) rather than suppressing a legitimate ledger-based
// fire — an Edge-side read blip must never take replenishment down.
func (m *ThresholdMonitor) linesideDecisionTotal(ctx context.Context, payload string) (edgeAdjusted, ledgerTotal int, usedEdge bool, err error) {
	ledgerTotal, err = m.readTotal(ctx, payload)
	if err != nil {
		return 0, 0, false, err
	}
	// No engine/DB/inventory (pure unit harness): ledger only.
	if m.eng == nil || m.eng.db == nil || m.eng.inventoryService == nil {
		return ledgerTotal, ledgerTotal, false, nil
	}

	reports, rerr := m.eng.db.ListLinesideReportsForPayload(payload)
	if rerr != nil {
		m.eng.logFn("threshold_monitor: R1 list reports for %s: %v (deciding off the ledger for this eval)", payload, rerr)
		return ledgerTotal, ledgerTotal, false, nil
	}
	if len(reports) == 0 {
		return ledgerTotal, ledgerTotal, false, nil
	}
	ledgerByNode, lerr := m.eng.inventoryService.LinesideLedgerByNode(ctx, payload)
	if lerr != nil {
		m.eng.logFn("threshold_monitor: R1 ledger-by-node(%s): %v (deciding off the ledger for this eval)", payload, lerr)
		return ledgerTotal, ledgerTotal, false, nil
	}

	// Adjustment = sum over FRESH reported nodes of (edge view − ledger view).
	// Stale nodes fall back to the ledger (no adjustment) and are flagged.
	now := time.Now()
	adjustment := 0
	freshNodes := 0
	for _, r := range reports {
		age := now.Sub(r.ReportedAt)
		if age >= linesideReportStaleness {
			m.eng.logFn("threshold_monitor: R1 STALE report — station=%s node=%s payload=%s reported %s ago (>= %s); falling back to the ledger for this node",
				r.Station, r.CoreNodeName, payload, age.Round(time.Second), linesideReportStaleness)
			continue
		}
		edgeNode := r.BinUOP + r.BucketQty
		ledgerNode := ledgerByNode[r.CoreNodeName]
		adjustment += edgeNode - ledgerNode
		freshNodes++
	}
	if freshNodes == 0 {
		return ledgerTotal, ledgerTotal, false, nil
	}
	return ledgerTotal + adjustment, ledgerTotal, true, nil
}

// auditLinesideDecision logs, per binding, any case where the pure ledger total
// and the Edge-adjusted total would reach DIFFERENT firing decisions — the
// permanent R1 audit trail (formerly the shadow "disagreement" log). It names the
// source that actually decided (the resolved lineside_decision_mode) and prints
// both totals, so a plant can reconcile every divergence after the fact. It is a
// no-op when no fresh report moved the total (the two totals are then identical).
func (m *ThresholdMonitor) auditLinesideDecision(payload string, bindings []thresholdEntry, ledgerTotal, edgeTotal int, usedEdge bool) {
	if !usedEdge || m.eng == nil {
		return
	}
	decidedBy := m.decisionMode()
	for _, b := range bindings {
		if b.threshold <= 0 {
			continue
		}
		ledgerBelow := ledgerTotal < b.threshold
		edgeBelow := edgeTotal < b.threshold
		if ledgerBelow == edgeBelow {
			continue
		}
		m.eng.logFn("threshold_monitor: R1 lineside AUDIT — payload=%s loader=%s threshold=%d: ledger total=%d would %s, edge-adjusted total=%d would %s; DECIDING OFF %s.",
			payload, b.coreNodeName, b.threshold,
			ledgerTotal, fireVerb(ledgerBelow),
			edgeTotal, fireVerb(edgeBelow),
			decidedBy)
	}
}

// OnLinesideReports is the report-arrival trigger. Called by the messaging layer
// after a batch of Edge lineside reports is upserted, for each payload that
// appeared. In edge_reports mode the fresh reports are a legitimate new fire
// trigger (the SNF3 case — the ledger stays stocked while the line starves, so
// the report is the only signal that reveals the truth), so it runs a full
// evaluation off the edge-adjusted total. In ledger mode it stays audit-only —
// exact pre-R1 behavior: log any disagreement, decide nothing (a report never
// moves the ledger, so nothing new would fire anyway). Reads-only w.r.t. the bins
// ledger.
func (m *ThresholdMonitor) OnLinesideReports(payloadCodes []string) {
	edgeMode := m.decisionMode() == linesideModeEdgeReports
	seen := make(map[string]struct{}, len(payloadCodes))
	for _, p := range payloadCodes {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		if edgeMode {
			m.evaluatePayload(p, "lineside_report")
			continue
		}
		m.auditOnlyForPayload(p)
	}
}

// auditOnlyForPayload runs the ledger-vs-edge audit for one payload WITHOUT
// touching the fire gate — the ledger-mode report-arrival behavior (pre-R1
// shadow: compare both ways, log the disagreement, decide nothing).
func (m *ThresholdMonitor) auditOnlyForPayload(payload string) {
	m.mu.Lock()
	bindings, monitored := m.thresholdsByPayload[payload]
	m.mu.Unlock()
	if !monitored || len(bindings) == 0 {
		return
	}
	edgeTotal, ledgerTotal, usedEdge, err := m.linesideDecisionTotal(context.Background(), payload)
	if err != nil {
		if m.eng != nil {
			m.eng.logFn("threshold_monitor: R1 audit readTotal(%s): %v", payload, err)
		}
		return
	}
	m.auditLinesideDecision(payload, bindings, ledgerTotal, edgeTotal, usedEdge)
}

func fireVerb(below bool) string {
	if below {
		return "FIRE"
	}
	return "hold"
}
