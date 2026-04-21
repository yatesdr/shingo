package service

import (
	"shingocore/store"
)

// TestCommandService centralizes test-command row CRUD for the
// simulated-order workflow (handlers_test_orders.go). Handlers call
// TestCommandService instead of reaching through engine passthroughs
// to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods are thin delegates today.
type TestCommandService struct {
	db *store.DB
}

func NewTestCommandService(db *store.DB) *TestCommandService {
	return &TestCommandService{db: db}
}

// Create inserts a new test command row and populates its ID.
func (s *TestCommandService) Create(tc *store.TestCommand) error {
	return s.db.CreateTestCommand(tc)
}

// Get loads a test command by ID.
func (s *TestCommandService) Get(id int64) (*store.TestCommand, error) {
	return s.db.GetTestCommand(id)
}

// UpdateStatus records the current vendor state and detail string on
// a test command.
func (s *TestCommandService) UpdateStatus(id int64, vendorState, detail string) error {
	return s.db.UpdateTestCommandStatus(id, vendorState, detail)
}

// Complete marks a test command as finished.
func (s *TestCommandService) Complete(id int64) error {
	return s.db.CompleteTestCommand(id)
}

// List returns the most recent test commands, capped at limit rows.
func (s *TestCommandService) List(limit int) ([]*store.TestCommand, error) {
	return s.db.ListTestCommands(limit)
}
