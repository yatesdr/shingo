package protocol

import (
	"database/sql/driver"
)

// SwapMode is the typed canonical changeover/dispatch swap mode for a
// process node claim. Wraps string so it serializes natively over JSON /
// SQL while gaining compile-time distinction from raw strings and other
// enum-shaped string types (e.g. ClaimRole, Status).
type SwapMode string

// Canonical swap-mode constants shared by core and edge. Wire/DB values
// are byte-identical to the raw strings these replace.
const (
	SwapModeSimple             SwapMode = "simple"
	SwapModeSingleRobot        SwapMode = "single_robot"
	SwapModeTwoRobot           SwapMode = "two_robot"
	SwapModeTwoRobotPressIndex SwapMode = "two_robot_press_index"
	SwapModeSequential         SwapMode = "sequential"
	SwapModeManualSwap         SwapMode = "manual_swap"
)

// String satisfies fmt.Stringer.
func (m SwapMode) String() string { return string(m) }

// IsTwoRobot reports whether this mode requires two-robot coordination
// (paired supply/removal legs with sibling linking). Centralised here so
// new two-robot variants only need to be added in one place.
//
// Note: the edge-local "press_position" marker deliberately returns false —
// per-position claims fan out to the single-position builder, not the
// two-robot coordination path. See edge/engine/changeover.go:95-105.
func (m SwapMode) IsTwoRobot() bool {
	return m == SwapModeTwoRobot || m == SwapModeTwoRobotPressIndex
}

// Scan implements sql.Scanner for reading from a database column.
// Mirrors protocol.Status — accepts string or []byte; NULL becomes empty.
// No validation against AllSwapModes(); historical rows must still load.
func (m *SwapMode) Scan(v any) error {
	return ScanEnumNamed(m, v, "protocol.SwapMode.Scan")
}

// Value implements driver.Valuer for writing to a database column.
func (m SwapMode) Value() (driver.Value, error) {
	return ValueEnum(m)
}

// AllSwapModes returns every defined swap mode, used by table-driven
// tests for exhaustive coverage.
func AllSwapModes() []SwapMode {
	return []SwapMode{
		SwapModeSimple,
		SwapModeSingleRobot,
		SwapModeTwoRobot,
		SwapModeTwoRobotPressIndex,
		SwapModeSequential,
		SwapModeManualSwap,
	}
}
