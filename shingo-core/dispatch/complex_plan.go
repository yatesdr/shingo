package dispatch

import (
	"shingo/protocol"
)

// ComplexPlan is a complex order's resolved step sequence, as the reserve and the
// confirm take it: reserveComplexPlan soft-holds a distinct source bin per pickup
// (reconciling against what the order already holds), and the order's hand
// (handFromComplexPlan) makes them hard. The reserve selects against live state
// from the steps alone.
type ComplexPlan struct {
	// ResolvedSteps is the full step sequence after node resolution. The
	// dispatcher persists this as StepsJSON so HandleOrderRelease can replay
	// per-segment.
	ResolvedSteps []resolvedStep
	// Reanchored: the reserve moved a vacant dependent pickup to a carrier at
	// its claim's inbound source (Allocator.reanchorVacantPickup). The caller
	// persists ResolvedSteps, as it does a widened plan.
	Reanchored bool
}

// BuildComplexPlan wraps already-resolved steps as a plan. Pure.
func BuildComplexPlan(steps []resolvedStep) *ComplexPlan {
	return &ComplexPlan{ResolvedSteps: steps}
}

// pickupNeed is one pickup step the plan-time reserve considers: its index in the
// original step list (recorded in order_bins.step_index so junction rows stay
// stably ordered) and whether it is a POTENTIAL relay re-grab (an earlier step
// dropped at this node).
//
// The relay rule is "an earlier dropoff at N AND N empty at reserve time (the
// bin hasn't relayed there yet)". potentialRelay carries the first, pure half —
// derivable from the step list alone. The reserve resolves the second half with
// live node state: a potential-relay node that STILL holds a bin is the real
// source it is (a swap's evac: the line still holds the old bin), not a skipped
// relay; only a potential-relay node that is empty at reserve is an actual
// re-grab. "Empty" is empty at the re-grab's step, which binsAtStep answers: a
// bin one of this order's own earlier pickups takes is gone before the re-grab
// runs, so it does not make the node a source (keep-staged combined collects the
// kept bin off inbound staging before staging its own carrier there).
//
// Splitting the rule this way keeps the pure half unit-testable without a DB
// while the reserve owns the live half — and leaves exactly one relay
// discriminator (this) in the tree.
type pickupNeed struct {
	stepIndex      int
	step           resolvedStep
	potentialRelay bool
	// kind is what the pickup is: a removal (it lifts whatever stands on the
	// order's own line), an empty supply (an Empty-flagged pickup) or a part
	// supply. One value; findAvailableForNeed selects by it.
	kind pickupKind
	// setDown is the node the pickup's carrier is next set down on (its next
	// dropoff), "" when none: the bind asks the height wanted there.
	setDown string
}

// pickupKind is what a pickup fetches.
type pickupKind int

const (
	// pickupPartSupply fetches a carrier of the step's part (the default).
	pickupPartSupply pickupKind = iota
	// pickupEmptySupply fetches an empty carrier (step.Empty).
	pickupEmptySupply
	// pickupRemoval lifts whatever stands on the order's own line node.
	pickupRemoval
)

// complexPickups returns every pickup step with its index and potential-relay
// flag — the full set the plan-time reserve walks, and the SINGLE relay
// discriminator in the tree. A pickup at N is a potential relay re-grab iff an
// EARLIER step drops at N (the order parked a bin there to re-collect); the
// reserve confirms an ACTUAL relay only when N is also empty at reserve. Pure over
// the step list and the order's process node — no live node state — which is
// why the fixtures in distinct_bin_pure_test.go can pin it without a DB. Each
// need also says what the pickup is (a removal at processNode, an empty supply,
// a part supply) and where its carrier is next set down.
func complexPickups(steps []resolvedStep, processNode string) []pickupNeed {
	dropped := make(map[string]bool, len(steps))
	var out []pickupNeed
	for i, s := range steps {
		switch s.Action {
		case protocol.ActionDropoff:
			if s.Node != "" {
				dropped[s.Node] = true
			}
		case protocol.ActionPickup:
			kind := pickupPartSupply
			switch {
			case isRemovalPickup(s, processNode):
				kind = pickupRemoval
			case s.Empty:
				kind = pickupEmptySupply
			}
			out = append(out, pickupNeed{stepIndex: i, step: s, potentialRelay: s.Node != "" && dropped[s.Node],
				kind: kind, setDown: nextDropoffResolved(steps, i+1)})
		}
	}
	return out
}
