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
// most. The PAIR RULE (shingo-core/dispatch/complex_pair.go) decides whether a
// coordinated swap dispatches at all: both legs are admitted in one scanner
// pass or neither is, which is what stops a line stranding empty (ALN_003,
// 2026-06-03). It serves two_robot, two_robot_press_index and anything added
// later, and there is not one SwapMode reference in it. It reads the PAIR
// STRUCTURE — does this order name a sibling — and the leg's own steps, and
// nothing else. The most dangerous decision in the swap path is mode-blind, so
// a gate that claims it cannot be has to explain why it is harder than that
// one.
//
// THE PREVIOUS WORKED EXAMPLE WAS swapLegHoldVerdict, and it is worth saying
// what happened to it. It was a mode-blind admission gate too, and correctly
// so — three faces, all reading the steps. All three are deleted anyway,
// because they shared a mistake the mode-blindness could not catch: they were
// gating DISPATCH, and dispatch does not move material for a swap leg. Every
// leg opens with a WAIT, so dispatch parks a robot under its node and the bins
// move at RELEASE. Reading a property instead of a mode name keeps a gate
// correct across modes; it does not make the gate necessary.
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
// forklift driver works, and the code knew it long before it said so:
// plantspec.Claim's loader predicate was documented as "a forklift-managed
// loader/unloader claim" while being named IsManualSwap after the field it
// read; store/processes.PayloadsForLoader implements the word "loader" as
// WalkOpts{SwapMode: manual_swap}; and domain.Loader.SynthClaim stamps this
// mode onto an in-memory claim for a thing that has no swap at all, purely so
// the branches engage. Every author wrote "loader" in the comment and
// "manual_swap" in the code, so the concept had no name and a reader had to
// find the sites and infer it.
//
// Ask the loader question of the claim — domain.NodeClaim.IsLoaderNode on the
// Edge, plantspec.Claim.IsLoader on Core — not of this field. Those two are the
// only permitted readers, and a drift test enforces it.
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

// ConfigurableSwapModes returns every swap mode that may be PERSISTED on a
// style node claim: AllSwapModes minus SwapModeSimple and SwapModeManualSwap.
// Both are retired as configurable modes and for different reasons.
//
//	simple       survives ONLY as a runtime CycleMode descriptor for the
//	             node-empty downgrade.
//	manual_swap  survives ONLY as a value SynthClaim STAMPS. A loader window is
//	             Core-owned topology: Core sends the loader set on every
//	             node-list sync, domain.Loader.SynthClaim builds the claim the
//	             board and the load gate read, and nothing about that needs a
//	             row in style_node_claims. A stored copy is a SECOND AUTHORITY
//	             for the six facts Core owns, and the stored population is now
//	             zero — SetCoreLoaders moves any that appear to
//	             style_node_claims_quarantine. This is the write side of the
//	             same ownership move: the rows were removed first, then the
//	             door was shut behind them.
//
// THE WIRE LEG IS UNTOUCHED, DELIBERATELY. SwapModeManualSwap is still in
// AllSwapModes, still stamped by SynthClaim, still read by the operator board
// and by IsLoaderNode. Only PERSISTENCE is retired. Inventing a new wire field
// to carry the same fact to the same readers would be a rename with migration
// risk and no reader simplification.
//
// WHAT KEYS ON THIS SET, AND IT IS NOT THE DROPDOWN. Three server-side gates:
// the claim-upsert allowlist (store/processes.UpsertClaim), the input validator
// (domain.ValidateNodeClaim), and seeddev's raw INSERT gate
// (shingo-core/cmd/seeddev/seed_edge.go), which mirrors the allowlist by hand
// because it cannot import the edge module. That third one is easy to miss and
// load-bearing: drop a value from this set and seeddev refuses every
// plants/*.yaml that uses it, so local dev and the CI sim rigs stop seeding.
// That is exactly what dropping manual_swap would have done, and the fixtures
// KEEP the value — it is a plantspec fact that Core's loader seeding, simcalc
// and plantspec.Validate all read through Claim.IsLoader. seed_edge stops
// writing an Edge claim row for those entries instead, which is the honest
// re-cut: the fixture still describes a loader, the Edge just no longer stores
// one.
//
// The editor's Swap Mode dropdown does NOT key on this set. It is hardcoded
// HTML (www/templates/processes.html) that reads nothing at runtime, and it
// carries a hidden "simple" option so an old row still renders when opened.
// It used to DISAGREE about manual_swap as well — the dropdown omitted a value
// this set allowed — and www/processes_enum_drift_test.go carried a clause
// excusing exactly that. The two now agree about loaders, and the clause is
// gone with it. "What may be stored" and "what the editor offers" are still
// different questions; only the first one is this function's.
func ConfigurableSwapModes() []SwapMode {
	return []SwapMode{
		SwapModeSingleRobot,
		SwapModeTwoRobot,
		SwapModeTwoRobotPressIndex,
		SwapModeSequential,
	}
}
