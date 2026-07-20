package store

// Delegate file for the plant-claims Core mirror (store/plantclaims/).
// Preserves the *store.DB method surface; see plantclaims package docs.

import "shingocore/store/plantclaims"

// ReplacePlantClaims replaces the mirror for one process from a plant-claims
// message. See plantclaims.ReplaceProcess.
func (db *DB) ReplacePlantClaims(processID string, styles []plantclaims.StyleRow, claims []plantclaims.ClaimRow, staleGuardConfigGen int64) error {
	return plantclaims.ReplaceProcess(db.DB, processID, styles, claims, staleGuardConfigGen)
}

// WipePlantClaims drops every plant-claims mirror row. See plantclaims.WipeAll.
func (db *DB) WipePlantClaims() error {
	return plantclaims.WipeAll(db.DB)
}

// PlantClaimsDirtyIndex builds the payload → (process, style) dirty index from
// the mirror. See plantclaims.DirtyIndex.
func (db *DB) PlantClaimsDirtyIndex() (map[string][]plantclaims.ProcessKey, error) {
	return plantclaims.DirtyIndex(db.DB)
}
