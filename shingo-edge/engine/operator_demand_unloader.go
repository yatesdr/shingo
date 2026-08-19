package engine

import (
	"fmt"
	"log"

	"shingoedge/domain"
	"shingoedge/orders"
)

// unloaderHasUsableFullPresent is the consumer-side counterpart to the
// removed loaderHasUsableEmptyPresent: skips the U1 full-in retrieve when
// Core reports a full bin of the target payload already physically at the
// unloader.
//
// It answers with the read's outcome alongside the finding, because the
// question has three answers and the second and third used to be the same one:
//
//   - (true,  "ok")            — Core says a matching full is standing there.
//   - (false, "ok")            — Core says it is not.
//   - (false, <a failure>)     — nobody answered, so neither of the above is known.
//
// FetchNodeBins used to collapse transport error, non-200 and decode failure
// into "no rows", so a failed read looked like an empty floor and the U1 fired
// anyway. That is the same fault that produced the 2026-07-31 loader
// over-ordering incident, one file over.
//
// "not_configured" is deliberately a distinct outcome rather than a failure:
// an Edge with no Core telemetry has nobody to ask, permanently, and a caller
// that refused on it would be off forever rather than waiting a cycle.
func (e *Engine) unloaderHasUsableFullPresent(coreNodeName, payloadCode string) (present bool, outcome string) {
	if !e.coreClient.Available() {
		return false, "not_configured"
	}
	if coreNodeName == "" || payloadCode == "" {
		return false, "not_asked"
	}
	bins, reachable, err := e.coreClient.FetchNodeBins([]string{coreNodeName})
	outcome = OccupancyOutcome(reachable, err)
	if !reachable {
		return false, outcome
	}
	if len(bins) == 0 {
		return false, outcome
	}
	b := bins[0]
	return b.Occupied && b.PayloadCode == payloadCode, outcome
}

// occupancyUnverifiable reports whether an occupancy outcome means the read was
// attempted and failed — as opposed to answering, or to there being nobody to
// ask. Only the first is a reason to hold off: it is a transient the next event
// will clear, where "not_configured" is a standing deployment fact.
func occupancyUnverifiable(outcome string) bool {
	switch outcome {
	case "unreachable", "http_error", "decode_err", "unverifiable":
		return true
	}
	return false
}

// MaybeCreateUnloaderFullIn (U1 of the side-cycle model) is the consumer-side
// counterpart to the loader side's L1: it pulls a full FG bin to the unloader
// for the operator to process. Resolves the unloader as a consume *domain.Loader
// and routes through the shared reservation seam (never-2N).
//
// U2 (empty-out from the unloader to the supermarket) fires when the unloader
// operator taps CLEAR — driven off the clear itself (ClearBin →
// createUnloaderEmptyOut), not off this U1 completing, so a press/forklift-fed
// drain with no U1 still drains.
//
// Caller: ReleaseOrderWithLineside in operator_release.go (produce-role
// lineside release). The consume DemandSignal caller is gone (2026-08) with
// the kanban demand-signal route; fulls landing at FG storage no longer
// auto-trigger a U1. The seam still applies (never-2N).
func (e *Engine) MaybeCreateUnloaderFullIn(payloadCode string) {
	loader, err := e.loaderStore.LoaderForPayload(domain.PayloadCode(payloadCode), domain.RoleConsume, true)
	if err != nil || loader == nil {
		return
	}
	e.createUnloaderFullInViaSeam(loader, payloadCode)
}

// createUnloaderFullInViaSeam is the consume-side path that routes the U1 full-in
// through the SHARED reservation seam (withLoaderBudget, retrieveEmpty=false).
// The unloader is resolved as a *domain.Loader (role=consume), so the never-2N
// budget, in-flight count, and free-window assignment are the EXACT code the
// loader's L1 uses — one seam, no loader/unloader drift.
//
// One thing the seam does NOT subsume: it counts in-flight ORDERS, not parked BINS.
// The loader could drop its physical-presence check because its `want` is demand-
// netted by the threshold monitor; the unloader's want=1 is event-driven, so the
// usable-full-present guard stays — run here over the delivery windows before the seam.
func (e *Engine) createUnloaderFullInViaSeam(loader *domain.Loader, payloadCode string) {
	if loader == nil {
		return
	}
	lid := string(loader.ID())
	if loader.InboundSource() == "" {
		// Forklift/press-fed drain: no AMR source to pull a full from — the operator
		// (reach truck) feeds the windows directly. Skip auto-pull (no U1 retrieve);
		// nothing to queue. The empty-out on clear (outbound_dest) is independent and
		// still fires. Unloaders that DO set an inbound source keep auto-pulling.
		e.debugFn("side-cycle: unloader %s has no inbound_source — fed directly, skip U1 auto-pull", lid)
		return
	}
	pc := domain.PayloadCode(payloadCode)
	nodes, budget := loader.ReservationTarget("", pc, e.multiWindowFor(loader))
	if len(nodes) == 0 || budget <= 0 {
		return // this unloader doesn't serve the payload
	}
	// Physical parked-full guard — the order-counting seam can't see a full bin
	// parked without an in-flight order. Symmetric to the legacy usable-present check.
	for _, n := range nodes {
		present, outcome := e.unloaderHasUsableFullPresent(string(n), payloadCode)
		if present {
			e.debugFn("side-cycle: unloader %s window %s already holds a full (%s) — skipping U1",
				lid, n, payloadCode)
			return
		}
		// An unverifiable read is reported and NOT acted on here, deliberately.
		// withLoaderBudget below makes the same read and now refuses on it, so a
		// second refusal at this level could never be the one that decided —
		// both reads go to the same endpoint through the same client, and the
		// conditions that fail one fail the other. Pinned by
		// TestCreateUnloaderFullIn_HoldsWhenOccupancyReadFails, which proves the
		// U1 holds without this line. The log stays because a trace should say
		// the guard could not see, rather than implying it looked and found
		// nothing.
		if occupancyUnverifiable(outcome) {
			e.debugFn("side-cycle: unloader %s window %s occupancy=%s — the guard could not see; the seam's own read decides",
				lid, n, outcome)
		}
	}
	created, err := e.withLoaderBudget(loader, pc, 1, "", false, func(deliveryNodes []string) (int, error) {
		made := 0
		for _, deliveryNode := range deliveryNodes {
			node, nerr := e.db.GetProcessNodeByCoreNodeName(deliveryNode)
			if nerr != nil || node == nil {
				return made, fmt.Errorf("side-cycle: no process_node for unloader window %s: %w", deliveryNode, nerr)
			}
			nodeID := node.ID
			// U1 = a FULL (retrieve_empty=false) pulled from the unloader's inbound FG
			// supermarket (blank → Core global FIFO). autoConfirm MUST be false — the
			// operator processes the bin before U2 fires (same rule as L1).
			//
			// NO_DEMAND, decided against the code rather than the trace. Three
			// things say loader-family, not cell-family, and none is close:
			//
			//   - It is EVENT-driven, not level-driven. Its callers are "a full
			//     arrived at FG storage" and a lineside release — it reacts to
			//     material APPEARING, which is the opposite direction from a
			//     place needing material. There is no threshold and no edge.
			//   - It has no PROCESS grain. It resolves a *domain.Loader by
			//     payload, so there is no process_id to key a cell episode on.
			//   - want is a fixed 1, not a plan's order count.
			//
			// So nothing asked for this bin: a full showed up and the system
			// pulled it in, the same shape as the loader's opportunistic push.
			// If it were ever to become real demand that would be a column
			// value here, not a redesign.
			if _, cerr := e.orderMgr.CreateRetrieveOrder(
				&nodeID, false, 1, deliveryNode, loader.InboundSource(), "",
				"standard", payloadCode, false, true, orders.NoDemand(),
			); cerr != nil {
				return made, fmt.Errorf("side-cycle: create U1 loader=%s payload=%s: %w", lid, payloadCode, cerr)
			}
			made++
			e.recordL1Burst(deliveryNode, 1) // delivery-node-keyed, the same tripwire as L1
		}
		return made, nil
	})
	if err != nil {
		e.logFn("side-cycle: unloader %s seam full-in for %s failed after %d created: %v", lid, payloadCode, created, err)
		return
	}
	if created > 0 {
		log.Printf("side-cycle: %d U1 order(s) via seam for unloader %s payload %s", created, lid, payloadCode)
	}
}

// pushUnloadersViaSeam is the seam-based auto-push: it walks every consume loader
// in the aggregate and offers each allowed payload to the shared reservation seam.
// The seam's never-2N budget makes it idempotent, so it is safe on any window-free
// event or as a startup sweep — already-covered windows create nothing.
//
// A consume loader has a single mode: the window-queue DRAIN (operator). It pulls a
// full whenever a window frees and one is waiting — operator-paced via never-2N.
// The only thing skipped is a (dormant) consume threshold loader: consume-side UOP
// thresholds aren't emitted yet, so threshold-mode consume loaders are left for that
// future kanban work rather than auto-drained here.
func (e *Engine) pushUnloadersViaSeam() {
	loaders, err := e.loaderStore.Loaders(domain.RoleConsume)
	if err != nil {
		e.logFn("side-cycle: push-unloaders seam list: %v", err)
		return
	}
	for _, l := range loaders {
		if l.Replenishment() == domain.ReplenishmentThreshold {
			continue // dormant consume-threshold mode — no auto drain yet (future kanban)
		}
		for _, code := range l.PayloadSet() {
			e.createUnloaderFullInViaSeam(l, string(code))
		}
	}
}

// MaybePushUnloader is the consume-side auto-push: when a window frees (ClearBin
// or handleManualSwapCompletion U2-arrived) it offers every auto consume loader's
// payloads to the shared seam. The seam's never-2N budget makes the sweep
// idempotent, so already-full windows create nothing — which is why nodeID is now
// only a (currently unused) efficiency hint and the old node→loader filter is gone.
func (e *Engine) MaybePushUnloader(_ int64) {
	e.pushUnloadersViaSeam()
}

// SweepPushUnloaders runs the consume auto-push sweep on Edge startup (after
// registration ack). Catches windows that became free while Edge was offline.
// The CAS guard serializes a re-register storm so concurrent sweeps don't stack.
func (e *Engine) SweepPushUnloaders() {
	if !e.sweepingUnloaders.CompareAndSwap(false, true) {
		return // a sweep is already running — a re-register storm must not stack them
	}
	defer e.sweepingUnloaders.Store(false)
	e.pushUnloadersViaSeam()
}
