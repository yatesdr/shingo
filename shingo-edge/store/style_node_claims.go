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
func (db *DB) GetStyleNodeClaimByNode(styleID int64, coreNodeName string) (*processes.NodeClaim, error) {
	return processes.GetClaimByNode(db.DB, styleID, coreNodeName)
}

// UpsertStyleNodeClaim inserts or updates a claim and returns the row id.
func (db *DB) UpsertStyleNodeClaim(in processes.NodeClaimInput) (int64, error) {
	return processes.UpsertClaim(db.DB, in)
}

// DeleteStyleNodeClaim removes a claim row by id.
func (db *DB) DeleteStyleNodeClaim(id int64) error {
	return processes.DeleteClaim(db.DB, id)
}
