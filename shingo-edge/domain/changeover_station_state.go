package domain

import (
	"database/sql/driver"

	"shingo/protocol"
)

// StationTaskState is the typed state for a changeover_station_task row.
// Wraps string so it serializes natively over JSON and SQL while gaining
// compile-time distinction from raw strings and the other enum-shaped
// state types (NodeTaskState, ChangeoverState, protocol.Status).
type StationTaskState string

// Canonical station-task state constants. The initial state "waiting" is
// written as a SQL literal at row insert time (service/changeover_service.go);
// these constants cover the Go-side comparison/update surface.
const (
	StationTaskWaiting    StationTaskState = "waiting"
	StationTaskInProgress StationTaskState = "in_progress"
	StationTaskSwitched   StationTaskState = "switched"
)

// String satisfies fmt.Stringer.
func (s StationTaskState) String() string { return string(s) }

// Scan implements sql.Scanner. Accepts string or []byte; NULL becomes
// the empty StationTaskState. Does NOT validate against known constants —
// historical rows from retired states must still load.
func (s *StationTaskState) Scan(v any) error {
	return protocol.ScanEnumNamed(s, v, "domain.StationTaskState.Scan")
}

// Value implements driver.Valuer for writing to a database column.
func (s StationTaskState) Value() (driver.Value, error) {
	return protocol.ValueEnum(s)
}
