package protocol

import "time"

// SubjectLinesideLevelReport — Edge → Core: a periodic per-consuming-node
// snapshot of lineside on-hand. A SUBJECT on the existing shingo.orders topic
// (the SubjectBinUOPDelta precedent — NOT a new topic, NOT a new consumer
// group). Value schema is ADDITIVE-only; an older Core that does not register
// this subject logs-and-ignores it (the SubjectRouter unknown-subject path), so
// the feed is a mixed-version no-op.
//
// IT IS A CHECKSUM AND DECIDES NOTHING. Every replenishment decision reads
// Core's own count (SystemUOPForPayload: bins plus lineside buckets, kept by the
// Edge's deltas), on every fire path. Core compares each row of this report
// against that replica on ingest and records a disagreement as a
// report_divergence episode (bin_uop_exception), shown on /inventory. Nothing
// heals from it: a divergence is corrected through the front door (a count
// correction), and a recurring cause is fixed on the delta path. A report that
// does not arrive changes no decision.
//
// It decided replenishment from 2026-07-24 (the lineside_decision_mode knob,
// default edge_reports) until the seat-count ruling of 2026-09-23 made Core's
// replica the one count. The ruling is recorded in the lineside comparison on
// Core (shingo-core/service/lineside_divergence.go).
const SubjectLinesideLevelReport = "inventory.lineside_level_report"

// LinesideLevelEntry is one consuming node's lineside on-hand for one payload,
// as the Edge sees it through its own counters.
//
//   - BinCount: 1 when the Edge has a carrier bound at the node, else 0.
//   - BinUOP:   the BOUND carrier's count AS OF FlushedSeq — its
//     remaining_uop_cached minus the counts the Edge's accumulator holds for it
//     unflushed — one bin per node, not a sum, and 0 when no bin is bound.
//   - BucketQty: active lineside bucket parts at the node for the payload, as
//     of the Edge's flushed bucket deltas the same way.
//   - BinID / BinEpoch: which carrier, and which generation of it
//     (active_bin_id / active_bin_epoch on the Edge's runtime row). nil BinID
//     with BinCount 1 is an Edge that predates these keys: Core compares no
//     carrier for that row. Omitted from the wire when no carrier is bound.
//   - FlushedSeq: the highest delta SequenceID the Edge has ALLOCATED for this
//     (bin, epoch) scope — inventory_delta_seq.next_seq, which despite its
//     name holds the last seq handed out (the first allocation returns 1), not
//     the next one. A seq is allocated immediately before its delta is enqueued
//     to the outbox; the reporter reads it, its counts and the accumulator's
//     unflushed counts under the accumulator's flush lock and enqueues the
//     report under the same lock, so BinUOP is exactly the count this seq's
//     net leaves and every delta it includes is ahead of the report. It
//     overstates by one only while an enqueue has just failed (the next flush
//     re-sends under seq+1), which can only hold a divergence back. 0 means
//     nothing was flushed for this generation. Core compares the count only
//     when its own last_seq for the scope equals this.
type LinesideLevelEntry struct {
	CoreNodeName string `json:"core_node_name"`
	PayloadCode  string `json:"payload_code"`
	BinCount     int    `json:"bin_count"`
	BinUOP       int    `json:"bin_uop"`
	BucketQty    int    `json:"bucket_qty"`
	BinID        *int64 `json:"bin_id,omitempty"`
	BinEpoch     int64  `json:"bin_epoch,omitempty"`
	FlushedSeq   int64  `json:"flushed_seq,omitempty"`
}

// LinesideLevelReport is the Edge's periodic (60s) batch of per-consuming-node
// lineside levels. Core upserts each entry into edge_lineside_reports keyed by
// (station, core_node_name, payload_code), latest-wins on ReportedAt, and
// compares the batch against its replica when at least one row moved.
type LinesideLevelReport struct {
	Station    string               `json:"station"`
	ReportedAt time.Time            `json:"reported_at"`
	Entries    []LinesideLevelEntry `json:"entries"`
}
