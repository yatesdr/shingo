package domain

import (
	"database/sql/driver"

	"shingo/protocol"
)

// ChangeoverState is the typed state for a process_changeovers row.
// Wraps string so it serializes natively over JSON and SQL while gaining
// compile-time distinction from raw strings and the other enum-shaped
// state types (NodeTaskState, StationTaskState, protocol.Status).
type ChangeoverState string

// Canonical changeover state constants. The initial state "active" is
// written as a SQL literal at row insert time (service/changeover_service.go);
// these constants cover the Go-side comparison/update surface.
const (
	ChangeoverActive    ChangeoverState = "active"
	ChangeoverCompleted ChangeoverState = "completed"
	ChangeoverCancelled ChangeoverState = "cancelled"
)

// String satisfies fmt.Stringer.
func (s ChangeoverState) String() string { return string(s) }

// Scan implements sql.Scanner. Accepts string or []byte; NULL becomes
// the empty ChangeoverState. Does NOT validate against known constants —
// historical rows from retired states must still load.
func (s *ChangeoverState) Scan(v any) error {
	return protocol.ScanEnumNamed(s, v, "domain.ChangeoverState.Scan")
}

// Value implements driver.Valuer for writing to a database column.
func (s ChangeoverState) Value() (driver.Value, error) {
	return protocol.ValueEnum(s)
}

// IsTerminal reports whether the changeover has reached a finalized state.
// Both Completed and Cancelled are terminal — the row no longer participates
// in active-changeover queries (GetActiveProcessChangeover filters them out
// at the SQL layer) and downstream node tasks should not be stamped further.
func (s ChangeoverState) IsTerminal() bool {
	return s == ChangeoverCompleted || s == ChangeoverCancelled
}
