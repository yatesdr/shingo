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
	// Reanchored: the reserve moved a bare on-deck empty pickup to an empty at
	// its claim's inbound source (Allocator.reanchorBareEmpty). The caller
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
}

// complexPickups returns every pickup step with its index and potential-relay
// flag — the full set the plan-time reserve walks, and the SINGLE relay
// discriminator in the tree. A pickup at N is a potential relay re-grab iff an
// EARLIER step drops at N (the order parked a bin there to re-collect); the
// reserve confirms an ACTUAL relay only when N is also empty at reserve. Pure over
// the step list — no live node state — which is why the fixtures in
// distinct_bin_pure_test.go can pin it without a DB.
func complexPickups(steps []resolvedStep) []pickupNeed {
	dropped := make(map[string]bool, len(steps))
	var out []pickupNeed
	for i, s := range steps {
		switch s.Action {
		case protocol.ActionDropoff:
			if s.Node != "" {
				dropped[s.Node] = true
			}
		case protocol.ActionPickup:
			out = append(out, pickupNeed{stepIndex: i, step: s, potentialRelay: s.Node != "" && dropped[s.Node]})
		}
	}
	return out
}
