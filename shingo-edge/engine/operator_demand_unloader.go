package engine

import (
	"fmt"
	"log"

	"shingoedge/domain"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
)

// unloaderHasUsableFullPresent is the consumer-side counterpart to the
// removed loaderHasUsableEmptyPresent: it finds the first target window on
// which Core reports a full bin of the target payload already physically
// standing, so the U1 full-in can be skipped. It answers from the loader's
// snapshot, matching each row to its window by NodeName.
//
// It says nothing when the snapshot cannot: the occupancy read failed
// (reachable=false — the caller then refuses anyway, and says so), or there is
// nobody to ask ("not_configured"), or no payload was named. Those used to be
// indistinguishable from "Core says it is not there": FetchNodeBins collapsed
// transport error, non-200 and decode failure into "no rows", so a failed read
// looked like an empty floor and the U1 fired anyway. That is the same fault
// that produced the 2026-07-31 loader over-ordering incident, one file over.
//
// "not_configured" is deliberately not a failure: an Edge with no Core
// telemetry has nobody to ask, permanently, and a caller that refused on it
// would be off forever rather than waiting a cycle.
func unloaderHasUsableFullPresent(snap *loaderSnapshot, nodes []string, payloadCode string) (window string, present bool) {
	if !snap.reachable || snap.occupancy == "not_configured" || payloadCode == "" {
		return "", false
	}
	for _, n := range nodes {
		for i := range snap.bins {
			b := snap.bins[i]
			if b.NodeName == n && b.Occupied && b.PayloadCode == payloadCode {
				return n, true
			}
		}
	}
	return "", false
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

// createUnloaderFullInViaSeam offers one payload to the unloader: the P=1 case
// of createUnloaderFullIns.
func (e *Engine) createUnloaderFullInViaSeam(loader *domain.Loader, payloadCode string) {
	e.createUnloaderFullIns(loader, []domain.PayloadCode{domain.PayloadCode(payloadCode)})
}

// unloaderOffer is one payload's reservation shape at an unloader.
type unloaderOffer struct {
	pay    string
	nodes  []string
	budget int
}

// createUnloaderFullIns is the consume-side path for the U1 full-in: one pass
// per unloader over the payloads offered. It takes the loader's reservation
// mutex once, reads the active orders and Core's occupancy for the loader's
// whole DeliveryNodes set once, and then decides each payload from that
// snapshot with the same count/cap/target step and loader_budget line as
// withLoaderBudget (decideLoaderBudget) — one seam, no loader/unloader drift.
// What each payload fires is added to the snapshot before the next payload is
// decided, so the pass sees its own orders exactly as a re-read would
// (TestUnloaderSweep_FirstFreeWindowAndSeesEarlierPayload).
//
// Cost: one Core node-bins GET per unloader that reaches the read, however many
// payloads and windows it has (TestUnloaderSweep_NodeBinsCallCount). It used to
// be one per window per payload for the guard plus one per payload for the seam.
//
// One thing the order count does NOT subsume: it counts in-flight ORDERS, not
// parked BINS. The loader could drop its physical-presence check because its
// `want` is demand-netted; the unloader's want=1 is event-driven, so the
// usable-full-present guard stays, per payload, before the count.
//
// Both early exits read nothing: an unloader with no inbound source, and an
// offer with no payload the unloader serves
// (TestUnloaderSweep_NoInboundSource_ZeroReads, _ZeroPayloads_ZeroReads).
//
// The RE-ENTRANCY RULE on withLoaderBudget applies here unchanged: the U1 is
// created, and EmitOrderCreated fires, with the loader's mutex held.
func (e *Engine) createUnloaderFullIns(loader *domain.Loader, payloads []domain.PayloadCode) {
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
	offers := make([]unloaderOffer, 0, len(payloads))
	for _, pc := range payloads {
		nodes, budget := loader.ReservationTarget("", pc, e.multiWindowFor(loader))
		if len(nodes) == 0 || budget <= 0 {
			continue // this unloader doesn't serve the payload
		}
		offers = append(offers, unloaderOffer{pay: string(pc), nodes: nodeIDStrings(nodes), budget: budget})
	}
	if len(offers) == 0 {
		return
	}
	readSet := nodeIDStrings(loader.DeliveryNodes())

	mu := e.loaderBudgetLock(lid)
	mu.Lock()
	defer mu.Unlock()

	snap, err := e.readLoaderSnapshot(readSet)
	if err != nil {
		// Fail closed — never fire into the dark when the order list is unavailable.
		for _, o := range offers {
			e.logFn("side-cycle: unloader %s seam full-in for %s failed after 0 created: reserve loader=%s: in-flight count: %v",
				lid, o.pay, lid, err)
		}
		return
	}
	// The guard could not see, and the decision below refuses on the same failed
	// read for every payload. Said once per unloader, so a trace records that the
	// guard was blind rather than implying it looked and found nothing.
	// TestCreateUnloaderFullIn_HoldsWhenOccupancyReadFails pins the refusal.
	if occupancyUnverifiable(snap.occupancy) {
		e.debugFn("side-cycle: unloader %s occupancy=%s — the guard could not see; the seam refuses every payload this pass",
			lid, snap.occupancy)
	}
	for _, o := range offers {
		// Physical parked-full guard — the order count can't see a full bin parked
		// without an in-flight order.
		if w, present := unloaderHasUsableFullPresent(&snap, o.nodes, o.pay); present {
			e.debugFn("side-cycle: unloader %s window %s already holds a full (%s) — skipping U1",
				lid, w, o.pay)
			continue
		}
		if !e.decideAndFireUnloaderFull(loader, &snap, readSet, o) {
			return
		}
	}
}

// decideAndFireUnloaderFull runs one payload's count→cap→target step against
// the snapshot, fires its U1, writes its loader_budget line, and folds what it
// created back into the snapshot. It reports false when the snapshot could not
// be brought up to date after a failed fire, and the pass must stop.
func (e *Engine) decideAndFireUnloaderFull(loader *domain.Loader, snap *loaderSnapshot, readSet []string, o unloaderOffer) bool {
	lid := string(loader.ID())
	d := decideLoaderBudget(snap, lid, o.pay, 1, o.budget, o.nodes, false)
	if !snap.reachable {
		d.logUnreachable(e, snap)
		return true
	}
	if d.toFire <= 0 {
		d.logNoFire(e, snap)
		return true
	}
	made, ferr := e.fireUnloaderFulls(loader, o.pay, d.targets)
	d.logFired(e, snap, len(made), ferr)
	if ferr != nil {
		e.logFn("side-cycle: unloader %s seam full-in for %s failed after %d created: %v", lid, o.pay, len(made), ferr)
		// A failed create may or may not have left its row; re-read the orders
		// (local, no Core call) rather than guess, and stop if that fails too.
		list, rerr := e.db.ListActiveOrdersByDeliveryNodeSet(readSet)
		if rerr != nil {
			e.logFn("side-cycle: unloader %s: re-read orders after a failed U1: %v — stopping this pass", lid, rerr)
			return false
		}
		snap.orders = list
		return true
	}
	for _, n := range made {
		snap.orders = append(snap.orders, storeorders.Order{DeliveryNode: n, PayloadCode: o.pay, RetrieveEmpty: false})
	}
	if len(made) > 0 {
		log.Printf("side-cycle: %d U1 order(s) via seam for unloader %s payload %s", len(made), lid, o.pay)
	}
	return true
}

// fireUnloaderFulls creates one U1 per target window and returns the windows
// it created on.
func (e *Engine) fireUnloaderFulls(loader *domain.Loader, payloadCode string, targets []string) ([]string, error) {
	lid := string(loader.ID())
	made := make([]string, 0, len(targets))
	for _, deliveryNode := range targets {
		nodeID, ok, rerr := e.resolveOwnWindow(deliveryNode,
			"side-cycle: unloader window %s has no process_node here — another Edge's window; skipped",
			"side-cycle: no process_node for unloader window %s: %w")
		if rerr != nil {
			return made, rerr
		}
		if !ok {
			// Same shape as the loader L1: one plant, several Edges — Core
			// broadcasts the loader config to every Edge, and this sweep
			// routinely walks a window served by another Edge. ErrNoRows is
			// the store's "not ours" answer — skip, don't fail. Measured 520
			// of these an hour on edge1 of the two-Edge sim stack (2026-09-07)
			// while edge2, which owns FGN_L2_01, logged none.
			continue
		}
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
		made = append(made, deliveryNode)
		e.recordL1Burst(deliveryNode, 1) // delivery-node-keyed, the same tripwire as L1
	}
	return made, nil
}

// pushUnloadersViaSeam is the seam-based auto-push: it walks every consume loader
// in the aggregate and offers all its allowed payloads to createUnloaderFullIns in
// one pass. The never-2N budget makes it idempotent, so it is safe on any
// window-free event or as a startup sweep — already-covered windows create nothing.
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
		e.createUnloaderFullIns(l, l.PayloadSet())
	}
}

// MaybePushUnloader is the consume-side auto-push: when a window frees (ClearBin
// or applyManualSwap U2-arrived) it offers every auto consume loader's
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
