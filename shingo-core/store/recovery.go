package store

// Phase 5 delegate file: recovery repair methods live in store/recovery/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"shingocore/store/recovery"
)

func (db *DB) RepairConfirmedOrderCompletion(orderID int64, places []RepairPlace, onPlaced OnPlaced) error {
	return recovery.RepairConfirmedOrderCompletion(db.DB, orderID, places, onPlaced)
}

func (db *DB) ReleaseTerminalBinClaim(binID int64) (int64, error) {
	return recovery.ReleaseTerminalBinClaim(db.DB, binID)
}

// RepairPlace is one cart a completion repair places (recovery.RepairPlace).
type RepairPlace = recovery.RepairPlace
