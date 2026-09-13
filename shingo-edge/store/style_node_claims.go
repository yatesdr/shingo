package store

// Phase 5b delegate file: style_node_claims CRUD lives in
// store/processes/. (Phase 6.0c folded the claims/ sub-package into
// processes/ — claims declare which core nodes a style needs.) This
// file preserves the *store.DB method surface and public struct names
// so external callers do not need to change.

import "shingoedge/store/processes"

// ListStyleNodeClaims returns every claim for a style.
func (db *DB) ListStyleNodeClaims(styleID int64) ([]processes.NodeClaim, error) {
	return processes.ListClaims(db.DB, styleID)
}

// GetStyleNodeClaim returns a single claim by id.
func (db *DB) GetStyleNodeClaim(id int64) (*processes.NodeClaim, error) {
	return processes.GetClaim(db.DB, id)
}

// GetStyleNodeClaimByNode returns a claim by its (style_id,
// core_node_name) pair.
// ClaimForLinesidePayload returns the single claim at a node matching a
// carrier's payload — "where does THIS bin belong", asked without reference to
// which style the process is on. nil when nothing matches or more than one does.
func (db *DB) ClaimForLinesidePayload(coreNodeName, payloadCode string) (*processes.NodeClaim, error) {
	return processes.ClaimForLinesidePayload(db.DB, coreNodeName, payloadCode)
}

func (db *DB) GetStyleNodeClaimByNode(styleID int64, coreNodeName string) (*processes.NodeClaim, error) {
	return processes.GetClaimByNode(db.DB, styleID, coreNodeName)
}

// IsPairedOnDeckNode reports whether coreNodeName is a press-index paired /
// on-deck position for any style in the process — a position that may hold
// only an empty carrier (A1 stamp guard, hop 2026-07-23).
func (db *DB) IsPairedOnDeckNode(processID int64, coreNodeName string) (bool, error) {
	return processes.IsPairedOnDeckNode(db.DB, processID, coreNodeName)
}

// UpsertStyleNodeClaim inserts or updates a claim and returns the row id.
func (db *DB) UpsertStyleNodeClaim(in processes.NodeClaimInput) (int64, error) {
	return processes.UpsertClaim(db.DB, in)
}

// DeleteStyleNodeClaim removes a claim row by id.
func (db *DB) DeleteStyleNodeClaim(id int64) error {
	return processes.DeleteClaim(db.DB, id)
}

// ListBackPositionNames is every position any of a process's live styles
// names as a partner slot — paired, second paired, inbound or outbound
// staging — in one query. The cell picture captions an unused position
// "back position" from this set, because a press's back slots are back
// slots under every style, not only the one running.
//
// LIVE CLAIMS, not just live styles. The style filter was here from the
// start and the claim filter was not, so a position a flow used to stage
// through kept its "back position" caption after the save that dropped it —
// a caption sourced from a row the flow no longer has.
func (db *DB) ListBackPositionNames(processID int64) (map[string]bool, error) {
	rows, err := db.Query(`
		SELECT c.paired_core_node, c.second_paired_core_node, c.inbound_staging, c.outbound_staging
		FROM style_node_claims c
		JOIN styles s ON s.id = c.style_id
		WHERE s.process_id = ? AND s.deleted_at IS NULL AND c.retired_at IS NULL`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var names [4]string
		if err := rows.Scan(&names[0], &names[1], &names[2], &names[3]); err != nil {
			return nil, err
		}
		for _, n := range names {
			if n != "" {
				out[n] = true
			}
		}
	}
	return out, rows.Err()
}
