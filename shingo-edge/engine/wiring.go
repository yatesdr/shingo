// wiring.go — Edge event-handler master registry.
//
// wireEventHandlers subscribes every EventBus consumer in shingo-edge's
// engine package. Handler implementations live in focused sibling
// files so this master file stays a top-down readable contract:
//
//   wiring_counter_delta.go    – CounterDelta UOP tracking, A/B
//                                cycling, lineside drain, auto-reorder/
//                                auto-relief decisions.
//   wiring_completion.go       – OrderCompleted dispatcher and the full
//                                completion chain (staged delivery,
//                                Order B, changeover release, manual
//                                swap, produce ingest, normal
//                                replenishment, keep-staged pre-stage),
//                                plus the OrderFailed counterpart for
//                                changeover-task error marking.
//   wiring_status_changed.go   – OrderStatusChanged reactions:
//                                sequential backfill on Order A
//                                in_transit, plus the two-robot
//                                auto-release-on-staged path.
//
// Stage 2A.2 (2026-04) split this file out of an 813-line monolith to
// match the Phase 2C precedent on the core side. The split is a pure
// move — no logic changes — and adding a new handler now means
// (a) write the function in the right sibling file, (b) subscribe it
// here. The master registry stays the one place to read the entire
// reactive contract.

package engine

import (
	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingoedge/store/processes"
)

// wireEventHandlers keeps process ownership in Edge and updates
// process-node runtime from order lifecycle events. Counter deltas
// still feed hourly production. The lambdas here are intentionally
// thin: they read the typed payload and defer to the real handler in
// a sibling file (handle*). Type assertions are gone — eventbus.SubscribeTyped
// extracts the concrete payload from TypedEvent[EventType, P].
func (e *Engine) wireEventHandlers() {
	if e.orderMgr != nil {
		e.orderMgr.OnStagedMessage = e.onStagedMessage
	}
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, CounterDeltaEvent]) {
		e.hourlyTracker.HandleDelta(evt.Payload)
		e.handleCounterDelta(evt.Payload)
	}, EventCounterDelta)

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCompletedEvent]) {
		e.handleNodeOrderCompleted(evt.Payload)
	}, EventOrderCompleted)

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderDeliveredEvent]) {
		e.handleNodeOrderDelivered(evt.Payload)
	}, EventOrderDelivered)

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		e.handleNodeOrderFailed(evt.Payload)
	}, EventOrderFailed)

	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderStatusChangedEvent]) {
		e.handleSequentialBackfill(evt.Payload)
		e.onReleaseStatusChanged(evt.Payload)
		e.returnCarrierLeftWaiting(evt.Payload)
	}, EventOrderStatusChanged)
}

// isInactivePairedNode reports whether a node is part of an A/B pair
// and is not the active-pull side. Both consume and produce branches
// of handleCounterDelta skip processing for the inactive half to
// avoid double-counting. Lives in the master file because it's read
// by the dispatcher itself, not by a single sibling — keeping it here
// avoids circular imports between siblings and makes the helper's
// audience obvious.
//
// NEVER A PRESS-INDEX SEAT. Its claim names the back position too, but the
// pair has no parked side: the index swaps carriers under the one press seat,
// which pulls for the pair's whole life, and nothing on the pair ever sets
// the bit. A press seat whose bit a tooling evacuate once cleared (a state a
// plant's database can hold) skipped every tick as inactive_produce for good
// (TestPressIndex_ADarkPressSeatStillCounts). Every other paired claim reads
// the bit as before.
func isInactivePairedNode(claim *processes.NodeClaim, runtime *processes.RuntimeState) bool {
	return claim.PairedCoreNode != "" && claim.SwapMode != protocol.SwapModeTwoRobotPressIndex && !runtime.ActivePull
}
