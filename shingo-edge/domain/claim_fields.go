package domain

import (
	"slices"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// ClaimHas reports whether a stored claim carries a value for the named
// field — the one accessor every reader of flowspec's tables uses to ask "is
// this field blank" (a Required check) or "is this field populated" (a
// Forbidden check), so the two questions cannot drift apart per reader.
//
// "Has a value" is the value-level rule the editor's drop note already uses:
// a non-empty string, a true flag, a non-zero number, a non-empty list. The
// carry-over disposition is the one exception — blank reads as replace, so
// replace is the absence of an opinion and does not count as a value.
//
// TOTAL OVER flowspec.Fields(), and TestClaimAccessorsAreTotal says so: a
// field added to the table without a case here reads as "never set", which
// would make a Required check refuse every claim and a Forbidden check refuse
// none. An unknown field is false for the same reason it is a test failure.
func ClaimHas(c *NodeClaim, f flowspec.Field) bool {
	if c == nil {
		return false
	}
	switch f {
	case flowspec.InboundStaging:
		return c.InboundStaging != ""
	case flowspec.OutboundStaging:
		return c.OutboundStaging != ""
	case flowspec.PairedCoreNode:
		return c.PairedCoreNode != ""
	case flowspec.OutboundDestination:
		return c.OutboundDestination != ""
	case flowspec.InboundSource:
		return c.InboundSource != ""
	case flowspec.SecondPairedCoreNode:
		return c.SecondPairedCoreNode != ""
	case flowspec.PayloadCode:
		return c.PayloadCode != ""
	case flowspec.AllowedPayloadCodes:
		return len(c.AllowedPayloadCodes) > 0
	case flowspec.UOPCapacity:
		return c.UOPCapacity != 0
	case flowspec.ReorderPoint:
		return c.ReorderPoint != 0
	case flowspec.ReorderPointSource:
		return c.ReorderPointSource != ""
	case flowspec.AutoReorder:
		return c.AutoReorder
	case flowspec.LinesideSoftThreshold:
		return c.LinesideSoftThreshold != 0
	case flowspec.Sequence:
		return c.Sequence != 0
	case flowspec.KeepStaged:
		return c.KeepStaged
	case flowspec.EvacuateOnChangeover:
		return c.EvacuateOnChangeover
	case flowspec.ChangeoverEvacNodes:
		return len(c.ChangeoverEvacNodes) > 0
	case flowspec.ChangeoverEvacDestination:
		return c.ChangeoverEvacDestination != ""
	case flowspec.ChangeoverCarryoverDisposition:
		return c.ChangeoverCarryoverDisposition != "" && c.ChangeoverCarryoverDisposition != CarryoverReplace
	case flowspec.ReuseCompatibleBins:
		return c.ReuseCompatibleBins
	case flowspec.IndexRobotSupplies:
		return c.IndexRobotSupplies
	case flowspec.AutoConfirm:
		return c.AutoConfirm
	case flowspec.AutoRequestPayload:
		return c.AutoRequestPayload != ""
	case flowspec.AutoPush:
		return c.AutoPush
	case flowspec.KeyRoute:
		return len(c.KeyRoute) > 0
	case flowspec.KeyTask:
		return c.KeyTask != ""
	}
	return false
}

// ClaimInputHas is ClaimHas for the write shape. The pointer-typed fields
// follow their contract: nil is "no opinion", which is not a value.
func ClaimInputHas(in NodeClaimInput, f flowspec.Field) bool {
	switch f {
	case flowspec.InboundStaging:
		return in.InboundStaging != ""
	case flowspec.OutboundStaging:
		return in.OutboundStaging != ""
	case flowspec.PairedCoreNode:
		return in.PairedCoreNode != ""
	case flowspec.OutboundDestination:
		return in.OutboundDestination != ""
	case flowspec.InboundSource:
		return in.InboundSource != ""
	case flowspec.SecondPairedCoreNode:
		return in.SecondPairedCoreNode != ""
	case flowspec.PayloadCode:
		return in.PayloadCode != ""
	case flowspec.AllowedPayloadCodes:
		return len(in.AllowedPayloadCodes) > 0
	case flowspec.UOPCapacity:
		return in.UOPCapacity != 0
	case flowspec.ReorderPoint:
		return in.ReorderPoint != 0
	case flowspec.ReorderPointSource:
		return in.ReorderPointSource != nil && *in.ReorderPointSource != ""
	case flowspec.AutoReorder:
		return in.AutoReorder != nil && *in.AutoReorder
	case flowspec.LinesideSoftThreshold:
		return in.LinesideSoftThreshold != 0
	case flowspec.Sequence:
		return in.Sequence != nil && *in.Sequence != 0
	case flowspec.KeepStaged:
		return in.KeepStaged != nil && *in.KeepStaged
	case flowspec.EvacuateOnChangeover:
		return in.EvacuateOnChangeover
	case flowspec.ChangeoverEvacNodes:
		return len(OptValue(in.ChangeoverEvacNodes)) > 0
	case flowspec.ChangeoverEvacDestination:
		return OptValue(in.ChangeoverEvacDestination) != ""
	case flowspec.ChangeoverCarryoverDisposition:
		d := OptValue(in.ChangeoverCarryoverDisposition)
		return d != "" && d != CarryoverReplace
	case flowspec.ReuseCompatibleBins:
		return in.ReuseCompatibleBins
	case flowspec.IndexRobotSupplies:
		return in.IndexRobotSupplies != nil && *in.IndexRobotSupplies
	case flowspec.AutoConfirm:
		return in.AutoConfirm
	case flowspec.AutoRequestPayload:
		return in.AutoRequestPayload != ""
	case flowspec.AutoPush:
		return in.AutoPush
	case flowspec.KeyRoute:
		return len(OptValue(in.KeyRoute)) > 0
	case flowspec.KeyTask:
		return OptValue(in.KeyTask) != ""
	}
	return false
}

// StrictSteadyModes are the modes whose Steady row is enforced IN FULL at
// both write paths — every Required entry refused blank and every Forbidden
// entry refused populated — by ValidateNodeClaim at the API and by
// UpsertClaim at the store, both through SteadyViolations, so the two cannot
// disagree.
//
// single_robot (D4). NOT sequential, which D4 also named, and the reason is
// measured rather than argued: the engine's runtime test corpus seeds
// sequential claims as its default shape at 141 sites in ~35 files — no A/B
// partner (87), outbound staging populated (36), inbound staging populated
// (11), no destination (7) — because a sequential claim runs steady-state
// replenishment by direct trips and needs none of those until a changeover.
// Enforcing the row at the store refuses every one of those seeds. The API
// already refuses the same claims (validateSwapModeRouting's sequential arm),
// no live sequential claim exists at either plant, and re-shaping 141
// fixtures is a change of its own; the store-side half stays pinned
// (store.TestStoreStillAcceptsWhatFlowspecRefuses) until the owner rules.
//
// Not the others either: two_robot, two_robot_press_index and manual_swap
// carry Forbidden entries that only the editor enforces today (D7, pinned in
// TestFlowspecMatchesValidateNodeClaim), and widening this list is a behaviour
// change with its own commit and its own plant query — a populated field the
// store starts refusing is a claim that refuses to re-save.
func StrictSteadyModes() []protocol.SwapMode {
	return []protocol.SwapMode{protocol.SwapModeSingleRobot}
}

// SteadyViolation is one field a claim lacks or carries against its mode's
// row: Need is Required for a blank Required field, Forbidden for a populated
// Forbidden one.
type SteadyViolation struct {
	Field flowspec.Field
	Need  flowspec.Need
}

// SteadyViolations lists what a claim in a STRICT mode gets wrong against
// flowspec.Steady, in flowspec.Fields order: every Required field that is
// blank and every Forbidden field that is populated. A mode outside
// StrictSteadyModes yields nothing — its rules are the arms the two write
// paths already carry by name — and so does a mode with no row of its own
// (blank, a typo, the in-memory press_position marker): the allowlist refuses
// it before anyone asks about its fields.
func SteadyViolations(in NodeClaimInput) []SteadyViolation {
	if !flowspec.Known(in.SwapMode) || !slices.Contains(StrictSteadyModes(), in.SwapMode) {
		return nil
	}
	row := flowspec.Steady(in.Role, in.SwapMode)
	var out []SteadyViolation
	for _, f := range flowspec.Fields() {
		switch row[f] {
		case flowspec.Required:
			if !ClaimInputHas(in, f) {
				out = append(out, SteadyViolation{Field: f, Need: flowspec.Required})
			}
		case flowspec.Forbidden:
			if ClaimInputHas(in, f) {
				out = append(out, SteadyViolation{Field: f, Need: flowspec.Forbidden})
			}
		}
	}
	return out
}
