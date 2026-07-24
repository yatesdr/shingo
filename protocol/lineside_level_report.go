package protocol

import "time"

// SubjectLinesideLevelReport — Edge → Core: a periodic per-consuming-node
// snapshot of lineside on-hand, for the R1 shadow read-model. A new SUBJECT on
// the existing shingo.orders topic (the SubjectBinUOPDelta precedent — NOT a
// new topic, NOT a new consumer group). Value schema is ADDITIVE-only; an older
// Core that does not register this subject logs-and-ignores it (the
// SubjectRouter unknown-subject path), so the feed is a mixed-version no-op.
//
// Purpose: the threshold monitor decides replenishment off Core's ledger
// (SystemUOPForPayload). When a bin strands `staged` at the line without
// binding, Core's ledger keeps the delivered count while Edge's own counters
// drain to the truth — the SNF3 CARRIER-0024 shape (Core 150 vs tile 46). This
// feed lets Core compute the same lineside term BOTH ways and log any
// disagreement that would change a firing decision. It is SHADOW: Core still
// decides off the ledger. The flip to deciding off these reports is a one-line
// switch once the shadow log shows agreement.
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
// shadow the monitor's lineside term.
type LinesideLevelReport struct {
	Station    string               `json:"station"`
	ReportedAt time.Time            `json:"reported_at"`
	Entries    []LinesideLevelEntry `json:"entries"`
}
