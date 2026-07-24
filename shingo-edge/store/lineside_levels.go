package store

import "fmt"

// LinesideLevel is one consuming node's current lineside on-hand, read from
// Edge's own authoritative counters (post the bin-ownership flip) for the R1
// shadow reporter.
type LinesideLevel struct {
	CoreNodeName string
	PayloadCode  string
	BinCount     int // 1 if a bin is bound at the node, else 0
	BinUOP       int // remaining_uop_cached for the bound bin
	BucketQty    int // active lineside bucket parts at the node
}

// ListLinesideLevels returns the current per-consuming-node lineside on-hand:
// for every process node whose ACTIVE claim is a consume claim carrying a
// payload, the bound-bin count + its remaining_uop_cached, and the node's
// active lineside bucket qty. Read-only; feeds the 60s R1 reporter.
//
// Bucket qty is the node's total ACTIVE bucket parts, attributed to the active
// claim's payload — a consuming node consumes one payload at a time, so this
// matches Core's per-(node, payload) bucket sum in the common case.
func (db *DB) ListLinesideLevels() ([]LinesideLevel, error) {
	rows, err := db.Query(`
		SELECT pn.core_node_name,
		       c.payload_code,
		       CASE WHEN r.active_bin_id IS NOT NULL THEN 1 ELSE 0 END AS bin_count,
		       r.remaining_uop_cached AS bin_uop,
		       COALESCE(bk.qty, 0) AS bucket_qty
		FROM process_node_runtime_states r
		JOIN process_nodes pn ON pn.id = r.process_node_id
		JOIN style_node_claims c ON c.id = r.active_claim_id
		LEFT JOIN (
			SELECT node_id, SUM(qty) AS qty
			FROM node_lineside_bucket
			WHERE state = 'active'
			GROUP BY node_id
		) bk ON bk.node_id = pn.id
		WHERE c.role = 'consume' AND c.payload_code != '' AND pn.core_node_name != ''`)
	if err != nil {
		return nil, fmt.Errorf("list lineside levels: %w", err)
	}
	defer rows.Close()
	var out []LinesideLevel
	for rows.Next() {
		var l LinesideLevel
		if err := rows.Scan(&l.CoreNodeName, &l.PayloadCode, &l.BinCount, &l.BinUOP, &l.BucketQty); err != nil {
			return nil, fmt.Errorf("scan lineside level: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
