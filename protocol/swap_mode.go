package protocol

import (
	"database/sql/driver"
	"errors"
)

// SwapMode is the typed canonical changeover/dispatch swap mode for a
// process node claim. Wraps string so it serializes natively over JSON /
// SQL while gaining compile-time distinction from raw strings and other
// enum-shaped string types (e.g. ClaimRole, Status).
type SwapMode string

// Canonical swap-mode constants shared by core and edge. Wire/DB values
// are byte-identical to the raw strings these replace.
const (
	// SwapModeSimple is retained as a CycleMode descriptor for the node-empty
	// downgrade (a claim with an empty head collapses to a delivery move tagged
	// "simple"); it is no longer a configurable claim mode — UpsertClaim and
	// plantspec.Validate reject it. See ConfigurableSwapModes.
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

// ErrInvalidSwapMode marks a claim rejected because its swap_mode is missing or
// not configurable — blank, the retired "simple", the in-memory-only
// "press_position" marker, a typo, or a stale import value. It's a client input
// problem, so HTTP handlers map it to 400 rather than 500. Callers wrap it with
// %w; the StyleService and store layers forward the error verbatim, so
// errors.Is sees it end to end.
var ErrInvalidSwapMode = errors.New("invalid swap_mode")

// ConfigurableSwapModes returns every swap mode that may be persisted on a
// style node claim: AllSwapModes minus SwapModeSimple. Simple is retired as a
// configurable mode — it survives ONLY as a runtime CycleMode descriptor for
// the node-empty downgrade, never as a stored claim mode. The claim-upsert
// allowlist, the editor's Swap Mode dropdown, and its drift test all key on
// this set so they can never disagree about what is selectable.
func ConfigurableSwapModes() []SwapMode {
	return []SwapMode{
		SwapModeSingleRobot,
		SwapModeTwoRobot,
		SwapModeTwoRobotPressIndex,
		SwapModeSequential,
		SwapModeManualSwap,
	}
}
