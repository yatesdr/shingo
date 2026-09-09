package protocol

import (
	"database/sql/driver"
	"errors"
)

// SwapMode is the typed canonical changeover/dispatch swap mode for a
// process node claim. Wraps string so it serializes natively over JSON /
// SQL while gaining compile-time distinction from raw strings and other
// enum-shaped string types (e.g. ClaimRole, Status).
//
// WHAT A SWAP MODE IS ALLOWED TO MEAN.
//
// A swap mode names a STEP-LIST SHAPE and nothing else. Gates read the steps,
// or a property declared on the claim — never the mode name.
//
// The worked example is already in the tree and it is the one that matters
// most. swapLegHoldVerdict (shingo-core/dispatch/swap_hold.go) is the
// safety-critical admission gate: it is what stops a line stranding empty
// (ALN_003, 2026-06-03) and what stops two bins landing on one press
// (Hopkinsville press-index, 2026-07). It serves two_robot,
// two_robot_press_index and sequential correctly, and there is not one
// SwapMode reference in it. It reaches its verdict by asking three questions
// of the steps — does this leg take the line's bin (legTakesLineBin), does it
// place one (legPlacesLineBin), does it secure its own replacement
// (legSecuresOwnReplacement). The most dangerous decision in the swap path is
// mode-blind, so a gate that claims it cannot be has to explain why it is
// harder than that one.
//
// The reason is not tidiness. A mode-name branch is a claim about every
// current AND future member of the set, made by an author who could only see
// the current members: add a mode and every such branch is silently wrong in
// whichever direction its author happened to write it, with nothing failing.
// A property read is a claim about the thing in front of it, so a new mode
// answers it or does not compile.
//
// COROLLARY: NODE KIND IS NOT A SWAP MODE.
//
// "manual_swap" is the standing counter-example, and it is worth reading
// before adding another. It does not name a choreography — it names a place a
// forklift driver works. plantspec.Claim.IsManualSwap documents itself as
// "a forklift-managed loader/unloader claim"; store/processes.PayloadsForLoader
// implements the word "loader" as WalkOpts{SwapMode: manual_swap}; and
// domain.Loader.SynthClaim stamps the mode onto an in-memory claim for a thing
// that has no swap at all, purely so those branches engage. The cost is that a
// reader who wants to know what a loader is has to find the sites and infer
// the concept. Ask the loader question of the claim
// (NodeClaim.IsLoaderNode), not of this field.
//
// HOW TO TELL AN ESSENTIAL MODE BRANCH FROM ONE THAT SHOULD MOVE.
//
// A branch may be deleted only when a property read replaces it. A branch that
// cannot be replaced is ESSENTIAL and stays — the honest residue is small and
// it is all one shape: code that is choosing or costing the step list itself.
// The builders pick which list to construct; simcalc's fleetMovesPerSwap costs
// each shape's floor crossings; ConfigurableSwapModes says which values may be
// persisted at all. Those read the mode because the mode is the answer, not
// because it is a convenient proxy for something else.
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
