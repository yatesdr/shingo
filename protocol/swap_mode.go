package protocol

import (
	"database/sql/driver"
	"fmt"
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

// Scan implements sql.Scanner for reading from a database column.
// Mirrors protocol.Status — accepts string or []byte; NULL becomes empty.
// No validation against AllSwapModes(); historical rows must still load.
func (m *SwapMode) Scan(v any) error {
	if v == nil {
		*m = ""
		return nil
	}
	switch x := v.(type) {
	case string:
		*m = SwapMode(x)
	case []byte:
		*m = SwapMode(x)
	default:
		return fmt.Errorf("protocol.SwapMode.Scan: cannot scan %T", v)
	}
	return nil
}

// Value implements driver.Valuer for writing to a database column.
func (m SwapMode) Value() (driver.Value, error) {
	return string(m), nil
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
