package store

// Phase 5 delegate file: recovery repair methods live in store/recovery/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"time"

	"shingocore/store/recovery"
)

func (db *DB) RepairConfirmedOrderCompletion(orderID, binID, toNodeID int64, staged bool, expiresAt *time.Time, onPlaced OnPlaced) error {
	return recovery.RepairConfirmedOrderCompletion(db.DB, orderID, binID, toNodeID, staged, expiresAt, onPlaced)
}

func (db *DB) ReleaseTerminalBinClaim(binID int64) (int64, error) {
	return recovery.ReleaseTerminalBinClaim(db.DB, binID)
}
