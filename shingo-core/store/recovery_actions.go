package store

// Phase 5 delegate file: recovery_actions CRUD lives in store/recovery/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"shingocore/store/recovery"
)

// RecoveryAction preserves the store.RecoveryAction public API.
type RecoveryAction = recovery.Action

func (db *DB) RecordRecoveryAction(action, targetType string, targetID int64, detail, actor string) error {
	return recovery.RecordAction(db.DB, action, targetType, targetID, detail, actor)
}

func (db *DB) ListRecoveryActions(limit int) ([]*RecoveryAction, error) {
	return recovery.ListActions(db.DB, limit)
}
