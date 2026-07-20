package sourceability

import (
	"database/sql"
	"fmt"

	"shingocore/store/plantclaims"
)

// Extra reads for the Core sourcing PAGE's drill-in. The page's verdicts come
// from the gated monitor snapshot (never a re-derivation); these only add
// context — which claims a style has, and how the pool splits free vs held — and
// are pure reads.

// PoolBreakdown is a payload's pool split: Free is the count dispatch could
// source now (the FindSourceFIFO predicate — unclaimed, unreserved, healthy);
// Held is the rest of the manifest-confirmed pool (claimed, reserved, or locked).
//
// FreeByNode names WHERE the free bins physically are, most-first. Free is the
// sum of its counts. The page renders this so an operator reading "Free 4" also
// sees "AMR Supermarket ×3 · ALN_012 ×1" — the count alone does not say where to
// go. Held is left as a per-payload total (its per-location drill-down is banked).
type PoolBreakdown struct {
	Free       int
	Held       int
	FreeByNode []NodeCount
}

// NodeCount is a free-bin count at one physical node.
type NodeCount struct {
	Node  string
	Count int
}

// LoadClaims returns the sourceability claims grouped by (process, style) from
// the mirror — the drill-in's claim list.
func LoadClaims(db *sql.DB) (map[plantclaims.ProcessKey][]plantclaims.ClaimRow, error) {
	_, claims, err := loadStylesAndClaims(db)
	return claims, err
}

// PoolBreakdownByPayload returns, per payload, the free-vs-held split over the
// manifest-confirmed pool on real enabled nodes. Free uses the identical
// predicate the computation nets against, so the drill-in's "free" always agrees
// with the verdict; Held is everything else in that pool (the held-vs-free view).
func PoolBreakdownByPayload(db *sql.DB) (map[string]PoolBreakdown, error) {
	// One row per (payload, node): the free count at that node and the pool
	// total there. Grouping by node — not just payload — is what lets the page
	// say WHERE the free bins are. The predicates are unchanged and still
	// identical to the computation's FindSourceFIFO filter, so a "free" here
	// always agrees with the verdict; the extra grouping key does not touch that.
	rows, err := db.Query(`
		SELECT b.payload_code,
		       n.name AS node,
		       COUNT(*) AS total,
		       COUNT(*) FILTER (
		         WHERE b.claimed_by IS NULL
		           AND b.locked = false
		           AND NOT EXISTS (SELECT 1 FROM reservations r WHERE r.bin_id = b.id AND r.state = 'pending')
		       ) AS free
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		WHERE b.payload_code <> ''
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND b.manifest_confirmed = true
		  AND b.status NOT IN ('staged', 'maintenance', 'flagged', 'retired', 'quality_hold')
		GROUP BY b.payload_code, n.name
		ORDER BY b.payload_code, free DESC, n.name`)
	if err != nil {
		return nil, fmt.Errorf("sourceability: pool breakdown: %w", err)
	}
	defer rows.Close()
	out := make(map[string]PoolBreakdown)
	for rows.Next() {
		var (
			payload, node string
			total, free   int
		)
		if err := rows.Scan(&payload, &node, &total, &free); err != nil {
			return nil, fmt.Errorf("sourceability: scan pool breakdown: %w", err)
		}
		pb := out[payload]
		pb.Free += free
		pb.Held += total - free
		// ORDER BY free DESC means the node with the most free bins is appended
		// first, so FreeByNode is already most-first for the page. A node that
		// holds only held bins (free = 0) is not listed as a free location.
		if free > 0 {
			pb.FreeByNode = append(pb.FreeByNode, NodeCount{Node: node, Count: free})
		}
		out[payload] = pb
	}
	return out, rows.Err()
}
