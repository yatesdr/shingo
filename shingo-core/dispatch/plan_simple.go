package dispatch

import "shingo/protocol"

// Order-builder foundation (Stages 1-2): express the "simple" order families as
// plans — resolvedStep lists over the closed pickup/dropoff/wait vocabulary —
// so that dispatch control flow can eventually branch on plan PROPERTIES rather
// than OrderType. Each builder is a pure function of the already-resolved
// endpoints; the plans are EMITTED at intake (on PlanningResult.Plan) but not
// yet consumed by dispatch. A differential test pins that each plan is
// fleet-equivalent to the order's existing transport tail before any dispatch
// path is unified (Stage 3).

// buildTransportPlan expresses a single-bin transport — retrieve, retrieve_empty,
// store, or move — as the canonical two-step plan: pick the (already-resolved)
// source bin up, drop it at the (already-resolved) delivery node. Fleet-equivalent
// to the TransportOrderRequest the transport tail builds today
// (dispatchToFleetCore: FromLoc=source, ToLoc=dest) — stepsToBlocks yields
// [pickup@source, dropoff@dest] → [JackLoad@source, JackUnload@dest].
//
// emptyPickup marks the pickup as an empty-carrier fetch (retrieve_empty) so that
// intent survives as STEP DATA (resolvedStep.Empty) — the property that replaces
// the OrderType==RetrieveEmpty scanner fork — rather than as the order's type. It
// does not change the fleet block shape (both actions map the same regardless of
// Empty); it is the sourcing/semantic distinction the later stages read.
func buildTransportPlan(sourceNode, deliveryNode string, emptyPickup bool) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionPickup, Node: sourceNode, Empty: emptyPickup},
		{Action: protocol.ActionDropoff, Node: deliveryNode},
	}
}
