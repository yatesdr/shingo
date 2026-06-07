package store

// Delegate for the plant-footprint cross-aggregate read (plan §15.D). Keeps
// the *store.DB method surface consistent with the other store sub-packages.

import "shingocore/store/footprint"

// GetFootprint returns the plant-footprint summary (cells/bins managed +
// load/unload velocity series).
func (db *DB) GetFootprint() (*footprint.Footprint, error) {
	return footprint.Get(db.DB)
}
