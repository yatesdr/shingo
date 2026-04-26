package store

// Phase 5 delegate file: inventory listing lives in store/inventory/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import "shingocore/store/inventory"

func (db *DB) ListInventory() ([]inventory.Row, error) {
	return inventory.List(db.DB)
}
