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
	"shingoedge/store/processes"
)

// wireEventHandlers keeps process ownership in Edge and updates
// process-node runtime from order lifecycle events. Counter deltas
// still feed hourly production. The lambdas here are intentionally
// thin: they unpack the event payload, then defer the real work to a
// handler in a sibling file (handle*).
func (e *Engine) wireEventHandlers() {
	e.Events.SubscribeTypes(func(evt Event) {
		if delta, ok := evt.Payload.(CounterDeltaEvent); ok {
			e.hourlyTracker.HandleDelta(delta)
			e.handleCounterDelta(delta)
		}
	}, EventCounterDelta)

	e.Events.SubscribeTypes(func(evt Event) {
		if completed, ok := evt.Payload.(OrderCompletedEvent); ok {
			e.handleNodeOrderCompleted(completed)
		}
	}, EventOrderCompleted)

	e.Events.SubscribeTypes(func(evt Event) {
		if failed, ok := evt.Payload.(OrderFailedEvent); ok {
			e.handleNodeOrderFailed(failed)
		}
	}, EventOrderFailed)

	e.Events.SubscribeTypes(func(evt Event) {
		if changed, ok := evt.Payload.(OrderStatusChangedEvent); ok {
			e.handleSequentialBackfill(changed)
		}
	}, EventOrderStatusChanged)
}

// isInactivePairedNode reports whether a node is part of an A/B pair
// and is not the active-pull side. Both consume and produce branches
// of handleCounterDelta skip processing for the inactive half to
// avoid double-counting. Lives in the master file because it's read
// by the dispatcher itself, not by a single sibling — keeping it here
// avoids circular imports between siblings and makes the helper's
// audience obvious.
func isInactivePairedNode(claim *processes.NodeClaim, runtime *processes.RuntimeState) bool {
	return claim.PairedCoreNode != "" && !runtime.ActivePull
}
