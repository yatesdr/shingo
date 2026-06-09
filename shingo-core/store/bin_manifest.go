package store

// Stage 2D delegate file: bin manifest Set/Confirm/Clear/Get + FIFO lookup
// live in store/bins/. FindStorageDestination is a cross-aggregate
// composition method that stays here. (Item 19 retired
// SetBinManifestFromTemplate from this surface — production callers
// route through service.BinManifestService.SetFromTemplate for audit.)

import (
	"fmt"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// SetBinManifest populates a bin's contents from a payload template.
func (db *DB) SetBinManifest(binID int64, manifestJSON string, payloadCode string, uopRemaining int) error {
	return bins.SetManifest(db.DB, binID, manifestJSON, payloadCode, uopRemaining)
}

// ConfirmBinManifest marks a bin's manifest as confirmed by an operator.
func (db *DB) ConfirmBinManifest(binID int64, producedAt string) error {
	return bins.ConfirmManifest(db.DB, binID, producedAt)
}

// ClearBinManifest empties a bin's manifest.
func (db *DB) ClearBinManifest(binID int64) error { return bins.ClearManifest(db.DB, binID) }

// GetBinManifest fetches a bin and parses its manifest.
func (db *DB) GetBinManifest(binID int64) (*bins.Manifest, error) {
	return bins.GetManifest(db.DB, binID)
}

// FindSourceBinFIFO finds the best unclaimed bin at an enabled storage node
// matching the given payload code, using FIFO ordering. excludeNodeID > 0
// skips bins at that node (pass destination to avoid same-node retrieve).
func (db *DB) FindSourceBinFIFO(payloadCode string, excludeNodeID int64) (*bins.Bin, error) {
	return bins.FindSourceFIFO(db.DB, payloadCode, excludeNodeID)
}

// FindStorageDestination finds the best storage node for a bin. Prefers nodes
// with existing bins of the same payload code, then empty nodes. Cross-aggregate
// composition (bins ↔ nodes) so it stays at this level.
//
// excludeNodeID > 0 skips the named node from the consolidation
// branch — callers pass the source node id so a store order never
// resolves its own pickup as the dropoff. Without this, the
// consolidation SQL picks the alphabetically-first node that has a
// matching bin, which can be the source itself when the source still
// holds the bin being stored. That landed on the same-node dispatch
// path pre-Round-3 and on Item C's "destination occupied" queue
// post-Round-3 — both are wrong, the real destination should be some
// OTHER node. Pass 0 to disable the exclusion (admin / non-source
// callers).
//
// The empty-fallback branch (second query) is naturally source-safe:
// the source has the bin being stored, so its bin-count is >= 1, and
// HAVING COUNT(sb.id) = 0 excludes it automatically.
func (db *DB) FindStorageDestination(payloadCode string, excludeNodeID int64) (*nodes.Node, error) {
	// Try consolidation: storage nodes that already have bins of the same payload code
	row := db.QueryRow(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			JOIN node_types snt ON snt.id = sn.node_type_id
			JOIN bins match_b ON match_b.node_id = sn.id AND match_b.payload_code = $1
			WHERE sn.enabled = true AND sn.is_synthetic = false
			  AND snt.code = 'STOR'
			  AND ($2 = 0 OR sn.id != $2)
			  AND sn.claimed_by IS NULL
			ORDER BY sn.name
			LIMIT 1
		)`, nodes.SelectCols, nodes.FromClause), payloadCode, excludeNodeID)
	n, err := nodes.ScanNode(row)
	if err == nil {
		return n, nil
	}

	// Fall back to any empty enabled physical node. No explicit
	// source exclusion needed — see function comment.
	row = db.QueryRow(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			JOIN node_types snt ON snt.id = sn.node_type_id
			LEFT JOIN bins sb ON sb.node_id = sn.id
			WHERE sn.enabled = true AND sn.is_synthetic = false
			  AND snt.code = 'STOR'
			  AND sn.claimed_by IS NULL
			GROUP BY sn.id
			HAVING COUNT(sb.id) = 0
			ORDER BY sn.name
			LIMIT 1
		)`, nodes.SelectCols, nodes.FromClause))
	return nodes.ScanNode(row)
}

// (Item 19 of the bin-as-truth refactor: SetBinManifestFromTemplate
// removed from the public *store.DB surface. Production callers
// route through BinManifestService.SetFromTemplate so the bin write
// audits via bin_uop_audit. The deleted function bypassed audit;
// post-Item-10 the audit timeline UI requires every manifest write
// to surface in bin_uop_audit. Test helpers that need an
// audit-bypass write path should call bins.SetManifest directly.)
