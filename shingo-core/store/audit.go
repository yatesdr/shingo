package store

// Phase 5 delegate file: audit-log CRUD lives in store/audit/. This file
// preserves the *store.DB method surface so external callers don't need
// to change. AddBinNote stays here as a thin convenience wrapper —
// it crosses naming concerns ("bin" entity type) and is small enough to
// keep at the outer level rather than push into audit/.

import (
	"time"

	"shingocore/store/audit"
)

func (db *DB) AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error {
	return audit.Append(db.DB, entityType, entityID, action, oldValue, newValue, actor)
}

func (db *DB) ListEntityAudit(entityType string, entityID int64) ([]*audit.Entry, error) {
	return audit.ListForEntity(db.DB, entityType, entityID)
}

// AddBinNote appends a typed note to a bin's audit trail.
func (db *DB) AddBinNote(binID int64, noteType, message, actor string) error {
	return db.AppendAudit("bin", binID, "note:"+noteType, "", message, actor)
}

// RollupBinUOPDeltaDay aggregates one UTC day of raw bin_uop_delta rows into
// bin_uop_delta_daily (v94). Called by the daily ticker in
// messaging/core_data_service.go, same family as the retention purges.
func (db *DB) RollupBinUOPDeltaDay(day time.Time) (int64, error) {
	return audit.RollupBinUOPDeltaDay(db.DB, day)
}

// PurgeOldBinUOPDelta deletes bin_uop_audit delta rows older than the
// retention window (D6: 90 days). The exceptions ledger and the daily roll-up
// carry the permanent record; see audit.PurgeOldBinUOPDelta.
func (db *DB) PurgeOldBinUOPDelta(keepDays int, now time.Time) (int64, error) {
	return audit.PurgeOldBinUOPDelta(db.DB, keepDays, now)
}
