package protocol

import "time"

// SubjectLinesideLevelReport — Edge → Core: a periodic per-consuming-node
// snapshot of lineside on-hand, for the R1 lineside read-model. A new SUBJECT
// on the existing shingo.orders topic (the SubjectBinUOPDelta precedent — NOT a
// new topic, NOT a new consumer group). Value schema is ADDITIVE-only; an older
// Core that does not register this subject logs-and-ignores it (the
// SubjectRouter unknown-subject path), so the feed is a mixed-version no-op.
//
// ⚠️ THIS FEED DECIDES REPLENISHMENT. It shadowed the ledger for part of a day
// on 2026-07-24 and has been authoritative since c20cf5aa. Under the default
// lineside_decision_mode=edge_reports the fire gate decides off the
// Edge-report-adjusted total; lineside_decision_mode=ledger reverts to the pure
// ledger, which is CONFIG, not code. Dropping these messages changes ordering
// decisions — see shingo-core/engine/threshold_monitor_lineside.go for the math
// and the staleness window.
//
// Purpose: the threshold monitor's own ledger (SystemUOPForPayload) keeps a
// bin's delivered count even when that bin strands `staged` at the line without
// binding, so it reads STOCKED while Edge's counters drain to the truth — the
// SNF3 CARRIER-0024 shape (Core 150 vs tile 46) — and silently suppresses
// ordering while the line starves. This feed carries Edge's view so Core can
// correct for that, and the ledger-vs-edge disagreement is logged permanently
// as the audit trail.
const SubjectLinesideLevelReport = "inventory.lineside_level_report"

// LinesideLevelEntry is one consuming node's lineside on-hand for one payload,
// as EDGE sees it via its own authoritative counters (post the bin-ownership
// flip that made Edge authoritative for lineside counts).
//
//   - BinCount: how many bins Edge has bound/present at the node for the payload.
//   - BinUOP:   Edge's remaining_uop_cached summed across those bins — the
//     number that diverges to 46 while Core holds 150. This is the
//     measure the firing-decision comparison uses on the bin side.
//   - BucketQty: active lineside bucket parts at the node for the payload.
type LinesideLevelEntry struct {
	CoreNodeName string `json:"core_node_name"`
	PayloadCode  string `json:"payload_code"`
	BinCount     int    `json:"bin_count"`
	BinUOP       int    `json:"bin_uop"`
	BucketQty    int    `json:"bucket_qty"`
}

// LinesideLevelReport is Edge's periodic (60s) batch of per-consuming-node
// lineside levels. Core upserts each entry into edge_lineside_reports keyed by
// (station, core_node_name, payload_code) and uses the fresh (< 3 min) rows to
// adjust the monitor's lineside term. A node with no fresh row contributes no
// adjustment and its ledger term stands, so a report that fails to arrive is a
// decision change, not a missing log line.
type LinesideLevelReport struct {
	Station    string               `json:"station"`
	ReportedAt time.Time            `json:"reported_at"`
	Entries    []LinesideLevelEntry `json:"entries"`
}
