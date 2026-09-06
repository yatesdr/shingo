package store

import "fmt"

// LinesideLevel is one consuming node's current lineside on-hand, read from
// Edge's own authoritative counters (post the bin-ownership flip) for the R1
// lineside reporter.
//
// Note the cardinality: BinCount is 0 or 1, the BOUND bin. Core's own per-node
// term sums EVERY bin whose node_id points at the node, bound or not, which is
// the whole reason this feed exists — three bins delivered with one bound reads
// as three on Core and one here.
//
// PayloadCode IS THE CARRIER'S, NOT THE CLAIM'S. This row used to take its
// identity from style_node_claims.payload_code via active_claim_id — the
// REQUESTED identity, which a changeover moves whether or not anything physical
// moves with it. Between 2026-09-02 and 2026-09-05 that made ALN_007 report
// 74871-6SA1A.06, a part of which zero existed plant-wide, against a carrier
// holding 7032 of 63125-6TA0A.06. Because the payload is the join key on Core,
// the wrong name did two things at once: it suppressed replenishment of a part
// that did not exist, and it silently removed the real part from the correction
// this feed exists to make.
type LinesideLevel struct {
	CoreNodeName string
	PayloadCode  string
	// PayloadKnown is whether the carrier's identity was established at all.
	// An unknown carrier is not an empty one, and it is not a report: the
	// reporter drops these rows rather than guessing, and Core's staleness arm
	// treats the absence as "no adjustment, the ledger stands" — the designed
	// degradation, and the only honest answer when nobody can say what is here.
	PayloadKnown bool
	BinCount     int // 1 if a bin is bound at the node, else 0
	BinUOP       int // remaining_uop_cached for the bound bin; 0 when none is bound
	BucketQty    int // active lineside bucket parts at the node, for THIS payload
}

// ListLinesideLevels returns the current per-consuming-node lineside on-hand:
// for every process node CONFIGURED as a consume node by the style its process
// is running, the bound-bin count + its remaining_uop_cached, and the node's
// active lineside bucket qty for the carrier's part. Read-only; feeds the 60s
// R1 reporter, whose output DECIDES replenishment on Core by default (see
// shingo-edge/engine/lineside_reporter.go).
//
// ── THREE GATES, AND EACH USED TO BE THE SAME ONE ────────────────────────
//
// WHETHER a node reports is a question about configuration: does the style this
// process is running consume at this node. That comes from the process's active
// style, which changes only when somebody changes it.
//
// WHAT the node holds is a question about a carrier: r.lineside_payload_code,
// written only by the doorway, only from a delivery envelope or a person, and
// cleared when the carrier leaves.
//
// WHETHER THERE IS ANYTHING TO SAY is active_bin_id or a live bucket. A node
// with neither has no on-hand to report and shipping a zero for it would assert
// something about a slot nobody has looked at.
//
// All three used to hang off r.active_claim_id, a single mutable pointer that
// eighteen paths write and most of them fill from the process's active style.
// One wrong pointer therefore changed the row's existence, its identity and its
// role filter together — which is why a stale pointer was not a cosmetic bug.
//
// NO COALESCE TO THE CLAIM, deliberately, and this is the one site where that
// rule is load-bearing rather than stylistic. A fallback here does not degrade
// to "slightly less accurate"; it mints an authoritative row under a payload
// nobody established, and Core stores it keyed on that name.
//
// The bucket term matches on payload_code so a bucket belonging to another
// payload is not summed under this one. Losing sight of it costs an adjustment
// Core's ledger still covers; attributing it costs a wrong authoritative row.
func (db *DB) ListLinesideLevels() ([]LinesideLevel, error) {
	rows, err := db.Query(`
		SELECT pn.core_node_name,
		       r.lineside_payload_code,
		       r.lineside_payload_known,
		       CASE WHEN r.active_bin_id IS NOT NULL THEN 1 ELSE 0 END AS bin_count,
		       CASE WHEN r.active_bin_id IS NOT NULL THEN r.remaining_uop_cached ELSE 0 END AS bin_uop,
		       COALESCE(bk.qty, 0) AS bucket_qty
		FROM process_node_runtime_states r
		JOIN process_nodes pn ON pn.id = r.process_node_id AND pn.deleted_at IS NULL
		JOIN processes p ON p.id = pn.process_id
		JOIN style_node_claims c
		  ON c.style_id = p.active_style_id AND c.core_node_name = pn.core_node_name
		LEFT JOIN (
			SELECT node_id, payload_code, SUM(qty) AS qty
			FROM node_lineside_bucket
			WHERE state = 'active'
			GROUP BY node_id, payload_code
		) bk ON bk.node_id = pn.id AND bk.payload_code = r.lineside_payload_code
		WHERE c.role = 'consume'
		  AND pn.core_node_name != ''
		  AND (r.active_bin_id IS NOT NULL OR COALESCE(bk.qty, 0) > 0)`)
	if err != nil {
		return nil, fmt.Errorf("list lineside levels: %w", err)
	}
	defer rows.Close()
	var out []LinesideLevel
	for rows.Next() {
		var l LinesideLevel
		if err := rows.Scan(&l.CoreNodeName, &l.PayloadCode, &l.PayloadKnown, &l.BinCount, &l.BinUOP, &l.BucketQty); err != nil {
			return nil, fmt.Errorf("scan lineside level: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
