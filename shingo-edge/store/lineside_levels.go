package store

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
)

// LinesideLevel is one consuming node's current lineside on-hand, read from the
// Edge's own counters for the lineside reporter. The report is a per-carrier
// checksum: Core compares each row against its own replica and decides nothing
// from it.
//
// Note the cardinality: BinCount is 0 or 1, the BOUND bin. Core may place more
// than one carrier at the node; which of them the Edge has bound is exactly
// what BinID tells Core, and a Core carrier at the seat that no row binds is
// one of the disagreements Core records.
//
// PayloadCode IS THE CARRIER'S, NOT THE CLAIM'S. This row used to take its
// identity from style_node_claims.payload_code via active_claim_id — the
// REQUESTED identity, which a changeover moves whether or not anything physical
// moves with it. Between 2026-09-02 and 2026-09-05 that made ALN_007 report
// SYN-PART09D.06, a part of which zero existed plant-wide, against a carrier
// holding 7032 of SYN-PART01E.06. Because the payload is the join key on Core,
// the wrong name did two things at once: it suppressed replenishment of a part
// that did not exist, and it silently removed the real part from the correction
// this feed exists to make.
type LinesideLevel struct {
	// NodeID is the Edge's process_nodes.id: the key the delta accumulator
	// holds this seat's unflushed bucket counts under.
	NodeID       int64
	CoreNodeName string
	PayloadCode  string
	// PayloadKnown is whether the carrier's identity was established at all.
	// An unknown carrier is not an empty one, and it is not a report: the
	// reporter drops these rows rather than guessing, which is the only honest
	// answer when nobody can say what is here.
	PayloadKnown bool
	BinCount     int // 1 if a bin is bound at the node, else 0
	BinUOP       int // remaining_uop_cached for the bound bin; 0 when none is bound
	BucketQty    int // active lineside bucket parts at the node, for THIS payload
	// BinID and BinEpoch are the bound carrier and its generation
	// (active_bin_id, active_bin_epoch); BinID is nil when none is bound.
	BinID    *int64
	BinEpoch int64
	// FlushedSeq is inventory_delta_seq.next_seq for the bound carrier's
	// (bin, epoch) scope: the LAST seq allocated, not the next one (the first
	// allocation returns 1), and a seq is allocated immediately before its delta
	// is enqueued to the outbox. 0 when nothing was allocated for the generation
	// or no carrier is bound. See protocol.LinesideLevelEntry.FlushedSeq.
	FlushedSeq int64
}

// ListLinesideLevels returns the current per-consuming-node lineside on-hand:
// for every process node CONFIGURED as a consume node by the style its process
// is running, the bound-bin count + its remaining_uop_cached, and the node's
// active lineside bucket qty for the carrier's part, and the bound carrier's
// identity with the last delta seq allocated for it. Read-only; feeds the 60s
// lineside reporter, a checksum Core compares against its replica (see
// shingo-edge/engine/lineside_reporter.go).
//
// ONE STATEMENT, pinned by TestListLinesideLevels_OneStatementForEverySeat. The
// carrier identity was already on the runtime row; the flushed seq is a LEFT
// JOIN on inventory_delta_seq's primary key (scope_kind, scope_key, epoch), with
// scope_key the bin id as text — the same key the accumulator allocates under.
//
// ── TWO GATES, AND EACH USED TO BE THE SAME ONE ──────────────────────────
//
// WHETHER a node reports is a question about configuration: does the style this
// process is running consume at this node. That comes from the process's active
// style, which changes only when somebody changes it.
//
// WHAT the node holds is a question about a carrier: r.lineside_payload_code,
// written only by the doorway, only from a delivery envelope or a person, and
// cleared when the carrier leaves.
//
// Both used to hang off r.active_claim_id, a single mutable pointer that
// eighteen paths write and most of them fill from the process's active style.
// One wrong pointer therefore changed the row's existence, its identity and its
// role filter together — which is why a stale pointer was not a cosmetic bug.
//
// AN EMPTY SEAT IS A ROW. There was a third gate, "is there anything to say":
// a node with no carrier bound and no bucket was left out. The owner ruled it
// out on 2026-09-24 ("the edges should always report even if nothing is
// happening"): an empty seat is something to say, and it is what lets Core see
// a carrier it places at a seat the Edge has nothing bound at. The runtime row
// is LEFT JOINed for the same reason, so a configured seat with no runtime row
// yet is listed as the empty seat it is.
//
// NO COALESCE TO THE CLAIM, deliberately, and this is the one site where that
// rule is load-bearing rather than stylistic. A fallback here does not degrade
// to "slightly less accurate"; it mints a row under a payload nobody
// established, Core stores it keyed on that name, and every comparison against
// Core's replica for that seat is then made against the wrong part.
//
// A RETIRED CLAIM IS NOT A LIVE ONE. `retired_at` is the flow composer's soft
// delete — a claim a changeover's history still points at is kept rather than
// dropped (store/processes/claims.go's liveClaims) — and the style+node join
// above would otherwise match one and mint a lineside row for a slot the style
// no longer claims.
//
// The bucket term matches on payload_code so a bucket belonging to another
// payload is not summed under this one. Losing sight of it costs an adjustment
// Core's mirror still covers; attributing it costs a wrong row.
func (db *DB) ListLinesideLevels() ([]LinesideLevel, error) {
	rows, err := db.Query(`
		SELECT pn.id, pn.core_node_name,
		       COALESCE(r.lineside_payload_code, ''),
		       COALESCE(r.lineside_payload_known, 0),
		       CASE WHEN r.active_bin_id IS NOT NULL THEN 1 ELSE 0 END AS bin_count,
		       CASE WHEN r.active_bin_id IS NOT NULL THEN r.remaining_uop_cached ELSE 0 END AS bin_uop,
		       COALESCE(bk.qty, 0) AS bucket_qty,
		       r.active_bin_id,
		       COALESCE(r.active_bin_epoch, 0),
		       COALESCE(sq.next_seq, 0) AS flushed_seq
		FROM process_nodes pn
		JOIN processes p ON p.id = pn.process_id
		JOIN style_node_claims c
		  ON c.style_id = p.active_style_id AND c.core_node_name = pn.core_node_name
		 AND c.retired_at IS NULL
		LEFT JOIN process_node_runtime_states r ON r.process_node_id = pn.id
		LEFT JOIN (
			SELECT node_id, payload_code, SUM(qty) AS qty
			FROM node_lineside_bucket
			WHERE state = 'active'
			GROUP BY node_id, payload_code
		) bk ON bk.node_id = pn.id AND bk.payload_code = r.lineside_payload_code
		LEFT JOIN inventory_delta_seq sq
		  ON sq.scope_kind = ? AND sq.scope_key = CAST(r.active_bin_id AS TEXT)
		 AND sq.epoch = r.active_bin_epoch
		WHERE c.role = 'consume'
		  AND pn.deleted_at IS NULL
		  AND pn.core_node_name != ''`, protocol.InvDeltaScopeBin)
	if err != nil {
		return nil, fmt.Errorf("list lineside levels: %w", err)
	}
	defer rows.Close()
	var out []LinesideLevel
	for rows.Next() {
		var l LinesideLevel
		var binID sql.NullInt64
		if err := rows.Scan(&l.NodeID, &l.CoreNodeName, &l.PayloadCode, &l.PayloadKnown, &l.BinCount, &l.BinUOP, &l.BucketQty,
			&binID, &l.BinEpoch, &l.FlushedSeq); err != nil {
			return nil, fmt.Errorf("scan lineside level: %w", err)
		}
		if binID.Valid {
			id := binID.Int64
			l.BinID = &id
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
