// wiring_lane_gate.go — the lane-gate release trigger set.
//
// A robot dwelling at a lane's wait point is waiting on ONE thing: the lane
// becoming safe to enter. Every event below is a way that can happen, and each
// one asks the dispatcher to re-evaluate the affected lane.
//
// The handler is deliberately dumb — it resolves "which lane just changed" and
// calls EvaluateLaneReleases, which derives everything else from live state.
// Nothing here decides anything, so a duplicate event is harmless, a dropped one
// costs only latency until the next firing, and the trigger set can be generous
// without any of these subscribers having to agree about ordering.

package engine

import (
	"shingo/protocol/eventbus"
)

// wireLaneGateHandlers registers the release evaluator on every event that can
// change a lane's occupancy. Called from wireEventHandlers.
//
// The set is EventBlockCompleted plus the events the fulfillment scanner already
// treats as occupancy signals — the same reasoning applies to both consumers: a
// parked order and a dwelling robot are waiting on the same lane facts.
func (e *Engine) wireLaneGateHandlers() {
	if e.dispatcher == nil {
		return
	}
	evaluate := func(laneIDs ...int64) {
		for _, id := range laneIDs {
			if id != 0 {
				e.dispatcher.EvaluateLaneReleases(id)
				// A COMPOUND LEG HELD AT THIS LANE IS WAITING ON THE SAME FACT.
				// It is not gate-staged and has no vendor order, so the evaluator
				// cannot see it — its candidate query keys on IsGateStaged. Its
				// dispatch path names a sibling's dropoff as its releaser, and with
				// no sibling in flight nothing was ever going to come back. These
				// are the events that actually free a lane, whoever caused it.
				//
				// Separate call rather than a branch inside the evaluator because
				// the two act on different POPULATIONS and do different things: the
				// evaluator appends a tail to a waybill the fleet already holds,
				// this dispatches a leg for the first time. (It is NOT because the
				// evaluator is gate_choreography-only — that gate was deleted by
				// F-05. See RedriveHeldCompoundLegs.)
				e.dispatcher.RedriveHeldCompoundLegs(id)
			}
		}
	}

	// PLACEMENT — the primary release signal. A store's dropoff block reaching
	// FINISHED is the moment its bin is down and its inbound mouth row is deleted
	// (handleStoreBlockCompleted → ReleaseInboundLaneForOrder), which is precisely
	// when it stops blocking a shallower entrant.
	//
	// REGISTRATION ORDER IS LOAD-BEARING, for the same reason as the A′ scanner
	// trigger: the bus dispatches synchronously in registration order, so this must
	// come AFTER the handleBlockCompleted subscription that drops the row. Ahead of
	// it, the evaluator would read the placer as still holding the mouth and decline
	// to release — correct but a firing late. wireLaneGateHandlers is called at the
	// end of wireEventHandlers to guarantee it.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BlockCompletedEvent]) {
		evaluate(e.dispatcher.LaneIDForNodeName(evt.Payload.Location))
	}, EventBlockCompleted)

	// PICKOUT — a bin left a lane slot: an outbound holder released its mouth row,
	// and the slot itself is now free. Both can unblock a dwelling entrant.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinEnteredTransitEvent]) {
		evaluate(e.dispatcher.LaneIDsForGateEvent(evt.Payload.FromNodeID))
	}, EventBinEnteredTransit)

	// BIN MOVED — the catch-all occupancy change (operator moves, corrections,
	// arrivals recorded outside a block completion). Evaluates the lane on either
	// side of the move, since a bin leaving one lane and landing in another changes
	// both.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
		ev := evt.Payload
		evaluate(
			e.dispatcher.LaneIDsForGateEvent(ev.FromNodeID),
			e.dispatcher.LaneIDsForGateEvent(ev.ToNodeID),
		)
	}, EventBinUpdated)

	// TERMINAL — a lane worker died holding a mouth row. TerminalizeOrder releases
	// its reservations in the same tx as the status write, so by the time these
	// fire the lane is already freer than the dwelling order last saw it.
	terminal := func(orderID int64) { evaluate(e.dispatcher.LaneIDsForOrder(orderID)...) }
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCompletedEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderCompleted)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCancelledEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderCancelled)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderFailed)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderSkippedEvent]) {
		terminal(evt.Payload.OrderID)
	}, EventOrderSkipped)
}
