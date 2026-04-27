package domain

import "time"

// TestCommand records a diagnostic / synthetic command issued from
// the operator UI (or the dispatcher's diagnostics path) so the
// post-hoc tooling can replay or audit what was sent. Created on
// command issuance, updated as state moves, completed when the
// vendor terminal state lands.
//
// Stage 2A.2 lifted this struct into domain/ so the diagnostics
// handler can return TestCommand objects without importing the
// shingo-core/store/diagnostics sub-package directly. The store
// package re-exports it via `type TestCommand = domain.TestCommand`.
type TestCommand struct {
	ID            int64
	CommandType   string
	RobotID       string
	VendorOrderID string
	VendorState   string
	Location      string
	ConfigID      string
	Detail        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CompletedAt   *time.Time
}
