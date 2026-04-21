package store

// Phase 5b delegate file: style_node_claims CRUD now lives in
// store/claims/. This file preserves the *store.DB method surface and
// public struct names so external callers do not need to change.

import "shingoedge/store/claims"

// StyleNodeClaim declares that a style needs a specific core node with
// a given payload and role. See the claims sub-package for full docs.
type StyleNodeClaim = claims.NodeClaim

// StyleNodeClaimInput is the input shape for UpsertStyleNodeClaim.
type StyleNodeClaimInput = claims.NodeClaimInput

// ListStyleNodeClaims returns every claim for a style.
func (db *DB) ListStyleNodeClaims(styleID int64) ([]StyleNodeClaim, error) {
	return claims.List(db.DB, styleID)
}

// GetStyleNodeClaim returns a single claim by id.
func (db *DB) GetStyleNodeClaim(id int64) (*StyleNodeClaim, error) {
	return claims.Get(db.DB, id)
}

// GetStyleNodeClaimByNode returns a claim by its (style_id,
// core_node_name) pair.
func (db *DB) GetStyleNodeClaimByNode(styleID int64, coreNodeName string) (*StyleNodeClaim, error) {
	return claims.GetByNode(db.DB, styleID, coreNodeName)
}

// UpsertStyleNodeClaim inserts or updates a claim and returns the row id.
func (db *DB) UpsertStyleNodeClaim(in StyleNodeClaimInput) (int64, error) {
	return claims.Upsert(db.DB, in)
}

// DeleteStyleNodeClaim removes a claim row by id.
func (db *DB) DeleteStyleNodeClaim(id int64) error {
	return claims.Delete(db.DB, id)
}
