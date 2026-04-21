package store

// Phase 5 delegate file: test_commands CRUD lives in store/diagnostics/.
// This file preserves the *store.DB method surface so external callers
// don't need to change.

import (
	"shingocore/store/diagnostics"
)

// TestCommand preserves the store.TestCommand public API.
type TestCommand = diagnostics.TestCommand

func (db *DB) CreateTestCommand(tc *TestCommand) error {
	return diagnostics.Create(db.DB, tc)
}

func (db *DB) UpdateTestCommandStatus(id int64, vendorState, detail string) error {
	return diagnostics.UpdateStatus(db.DB, id, vendorState, detail)
}

func (db *DB) CompleteTestCommand(id int64) error {
	return diagnostics.Complete(db.DB, id)
}

func (db *DB) GetTestCommand(id int64) (*TestCommand, error) {
	return diagnostics.Get(db.DB, id)
}

func (db *DB) ListTestCommands(limit int) ([]*TestCommand, error) {
	return diagnostics.List(db.DB, limit)
}

func (db *DB) ListActiveTestCommands() ([]*TestCommand, error) {
	return diagnostics.ListActive(db.DB)
}
