package dispatch

import (
	"shingo/protocol"
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

// buildUnloadOnlyPlan is the carried-bin recovery plan: put down what you are
// already carrying, here.
//
// NO PICKUP, and that is not an optimisation. The bin is on the robot's deck;
// its source node is the synthetic carrier node, which is a bookkeeping row and
// not a map point. A pickup step would send the robot to a place that does not
// exist, and a pickup at the DESTINATION would tell it to lift the bin it is
// about to set down.
//
// One block out of this, a JackUnload at deliveryNode, and the order completes
// the moment the deck reports empty there — which is also how Core learns where
// the bin ended up, through the ordinary arrival path rather than an inference.
func buildUnloadOnlyPlan(deliveryNode string) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionDropoff, Node: deliveryNode},
	}
}
