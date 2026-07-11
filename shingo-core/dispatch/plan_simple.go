package dispatch

import "shingo/protocol"

// Order-builder foundation: express the "simple" order families as plans —
// resolvedStep lists over the closed pickup/dropoff/wait vocabulary — so that
// dispatch control flow can branch on plan PROPERTIES rather than OrderType. Each
// builder is a pure function of the already-resolved endpoints. In this step the
// plans are EMITTED (on PlanningResult.Plan) but NOT consumed by dispatch and NOT
// persisted: the plain path re-finds its source and builds the fleet request from
// the order columns, so a simple plan has no reader yet. A differential test pins
// each plan fleet-equivalent to the transport tail; persisting + consuming the
// plan (one plan-shaped dispatch tail for simple and complex alike) is the
// unified-create follow-up.

// buildTransportPlan expresses a single-bin transport — retrieve, retrieve_empty,
// or move (the store family was removed) — as the canonical two-step plan: pick
// the (already-resolved) source bin up, drop it at the (already-resolved)
// delivery node. Fleet-equivalent to the TransportOrderRequest the transport tail
// builds today (dispatchToFleetCore: FromLoc=source, ToLoc=dest) — stepsToBlocks
// yields [pickup@source, dropoff@dest] → [JackLoad@source,
// JackUnload@dest].
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
