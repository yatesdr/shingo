package store

// Phase 5 delegate file: audit-log CRUD lives in store/audit/. This file
// preserves the *store.DB method surface so external callers don't need
// to change. AddBinNote stays here as a thin convenience wrapper —
// it crosses naming concerns ("bin" entity type) and is small enough to
// keep at the outer level rather than push into audit/.

import (
	"shingocore/store/audit"
)

// AuditEntry preserves the store.AuditEntry public API.
type AuditEntry = audit.Entry

func (db *DB) AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error {
	return audit.Append(db.DB, entityType, entityID, action, oldValue, newValue, actor)
}

func (db *DB) ListAuditLog(limit int) ([]*AuditEntry, error) {
	return audit.List(db.DB, limit)
}

func (db *DB) ListEntityAudit(entityType string, entityID int64) ([]*AuditEntry, error) {
	return audit.ListForEntity(db.DB, entityType, entityID)
}

// AddBinNote appends a typed note to a bin's audit trail.
func (db *DB) AddBinNote(binID int64, noteType, message, actor string) error {
	return db.AppendAudit("bin", binID, "note:"+noteType, "", message, actor)
}
