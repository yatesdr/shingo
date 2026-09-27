package dispatch

import (
	"shingocore/dispatch/plan"
)

// Order-builder foundation: express the "simple" order families as plans —
// resolvedStep lists over the closed pickup/dropoff/wait vocabulary — so that
// dispatch control flow can branch on plan PROPERTIES rather than OrderType. Each
// builder is a pure function of the already-resolved endpoints. dispatchToFleetCore
// builds the plain path's fleet request from buildTransportPlan. Intake no longer
// emits a copy of the plan (PlanningResult.Plan had no reader and was removed); a
// differential test pins the transport plan fleet-equivalent to what the scanner
// dispatches.

// buildTransportPlan expresses a single-bin transport as the canonical two-step
// plan: the planner's leg shape (plan.TransportLegs).
func buildTransportPlan(sourceNode, deliveryNode string, emptyPickup bool) []resolvedStep {
	return plan.TransportLegs(sourceNode, deliveryNode, emptyPickup)
}

// buildUnloadOnlyPlan is the carried-bin recovery plan: the planner's leg shape
// (plan.UnloadOnlyLegs).
func buildUnloadOnlyPlan(deliveryNode string) []resolvedStep {
	return plan.UnloadOnlyLegs(deliveryNode)
}
