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

// PurgeOldBinUOPDelta deletes bin_uop_ledger delta rows older than the
// retention window (D6: 90 days). The exceptions ledger and the daily roll-up
// carry the permanent record; see audit.PurgeOldBinUOPDelta.
func (db *DB) PurgeOldBinUOPDelta(keepDays int, now time.Time) (int64, error) {
	return audit.PurgeOldBinUOPDelta(db.DB, keepDays, now)
}

// RecordHoldDropped writes one hold_dropped exception for an Edge's dropped
// produce hold, once per envelope id; see audit.AppendHoldDropped.
func (db *DB) RecordHoldDropped(msgID, station, node, payloadCode string, qty int64, reason, actor, orderID string, occurredAt time.Time) (bool, error) {
	return audit.AppendHoldDropped(db.DB, msgID, station, node, payloadCode, qty, reason, actor, orderID, occurredAt)
}

// ListHoldDropRuns lists the hold_dropped exceptions since a time as runs
// (audit.HoldDropRun), latest first.
func (db *DB) ListHoldDropRuns(since time.Time, gap time.Duration, limit int) ([]audit.HoldDropRun, error) {
	return audit.ListHoldDropRuns(db.DB, since, gap, limit)
}

// ListDeclaredFullStamps lists the release_declared_full ledger rows since a
// time, newest first.
func (db *DB) ListDeclaredFullStamps(since time.Time, limit int) ([]audit.DeclaredFullStamp, error) {
	return audit.ListDeclaredFullStamps(db.DB, since, limit)
}
