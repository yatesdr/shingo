package store

// Phase 5 delegate file: corrections CRUD lives in store/inventory/
// (grouped there because corrections are ledger rows for the inventory
// aggregate). This file preserves the *store.DB method surface so
// external callers don't need to change.

import (
	"shingocore/store/inventory"
)

// Correction preserves the store.Correction public API.
type Correction = inventory.Correction

func (db *DB) CreateCorrection(c *Correction) error {
	return inventory.CreateCorrection(db.DB, c)
}

func (db *DB) ListCorrections(limit int) ([]*Correction, error) {
	return inventory.ListCorrections(db.DB, limit)
}

func (db *DB) ListCorrectionsByNode(nodeID int64, limit int) ([]*Correction, error) {
	return inventory.ListCorrectionsByNode(db.DB, nodeID, limit)
}

// ApplyBinManifestChanges applies corrections to a bin's manifest and
// records correction rows.
func (db *DB) ApplyBinManifestChanges(binID int64, corrections []*Correction) error {
	return inventory.ApplyBinManifestChanges(db.DB, binID, corrections)
}
