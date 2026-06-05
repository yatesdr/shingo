package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// countLoaderInFlightEmptyIn returns the number of non-terminal
// retrieve_empty orders inbound to the loader's CORE NODE for the payload.
// MaybeCreateLoaderEmptyIn uses this against ReorderPoint to top up the
// in-flight queue to (ReorderPoint - currentCount) instead of capping at
// one — operators get the full demand visible at once rather than one
// queue per demand signal.
//
// Keyed by core node (delivery_node), not process_node: a loader shared across
// styles/cells has many process_node rows for one physical slot, so a
// process-node count would miss empties staged against a sibling row and
// over-fire L1. See [[shingo_manual_swap_core_node_scoping]] / the orders-store
// docstring.
//
// Returns an error (not an in-band sentinel) on a DB read failure so
// tryCreateL1 can fail closed — fire nothing rather than into the dark when
// the order list is unavailable.
func (e *Engine) countLoaderInFlightEmptyIn(coreNodeName string, payloadCode string) (int, error) {
	n, err := e.countActiveOrdersAtNode(coreNodeName, func(o orders.Order) bool {
		return o.PayloadCode == payloadCode && o.RetrieveEmpty
	})
	if err != nil {
		return 0, fmt.Errorf("list active orders for node %s: %w", coreNodeName, err)
	}
	return n, nil
}

// MaybeCreateLoaderEmptyIn (L1 of the side-cycle model) creates a
// retrieve_empty order tracked at the loader for the given payload, if a
// matching loader exists and doesn't already have an in-flight empty-in.
// Called from ReleaseOrderWithLineside on consume-role releases under
// DispositionCaptureLineside: when the line operator declares a bin
// emptied, the loader gets a parallel "empty-in" demand so it stays in
// the workflow. (Pre-2026-04-29 this fired on REQUEST; that over-supplied
// the loader whenever the line later returned a partial.)
//
// L2 (filled-out to supermarket) is created when this order's bin reaches
// the loader and the operator confirms — see handleLoaderEmptyInCompletion.
//
// The retrieve_empty order's source is left to Core's planner
// (planRetrieveEmpty) which finds an unclaimed empty bin matching the bin
// type. Excludes the loader itself via the excludeNodeID guard (commit
// 7047c5a) so the loader isn't asked to source from itself.
func (e *Engine) MaybeCreateLoaderEmptyIn(coreNodeName, payloadCode string) {
	loader := e.findLoaderForDemand(coreNodeName, payloadCode)
	if loader == nil {
		return
	}
	// Each demand signal triggers a full sweep across all of the loader's
	// allowed payloads, not just the signaled one. A multi-payload loader
	// (e.g., A, B, C, D each with ReorderPoint=2) may have several payloads
	// in deficit at once; we want the queue to reflect that all at once so
	// the operator sees the full demand rather than discovering it one signal
	// at a time. The signaled payload is what tells us which loader to
	// evaluate; what to queue is computed per-payload from current state.
	//
	// UOP-threshold replenishment (C-push) — for any (loader, payload)
	// with replenish_uop_threshold > 0, Core is the source of truth.
	// Skip the legacy bin-count evaluation here; Core's
	// LoopBelowThresholdSignal goes through HandleLoopBelowThreshold
	// instead. The countLoaderInFlightEmptyIn guard on both paths is
	// the dedup contract for the race window where both signals arrive
	// near-simultaneously — do not remove or weaken either guard.
	for _, code := range loader.claim.AllowedPayloads() {
		if e.hasOptInLoaderThreshold(loader.node.CoreNodeName, code) {
			e.debugFn("kanban: HandleDemandSignal skip loader=%s payload=%s — C-push active",
				loader.node.CoreNodeName, code)
			continue
		}
		e.refillLoaderForPayload(loader, code)
	}
}

// findLoaderForDemand resolves the active-style produce loader for a legacy
// DemandSignal. It prefers the node Core named (coreNodeName) so a payload
// loaded at two separate loaders routes to the one the signal is about — the
// same protection HandleLoopBelowThreshold has on the threshold path. It
// falls back to payload-first-match when the signal carries no node, OR the
// named node has no matching active loader, so the most load-bearing path
// keeps working even if Core's DemandSignal node semantics differ from the
// assumption (fail-safe pending SME confirmation of those semantics).
func (e *Engine) findLoaderForDemand(coreNodeName, payloadCode string) *manualSwapNode {
	if coreNodeName != "" {
		var found *manualSwapNode
		err := processes.WalkClaims(e.db.DB, processes.WalkOpts{
			ActiveOnly:   true,
			Role:         protocol.ClaimRoleProduce,
			SwapMode:     protocol.SwapModeManualSwap,
			CoreNodeName: coreNodeName,
			PayloadCode:  payloadCode,
			ResolveNode:  true,
		}, func(ctx processes.WalkCtx) bool {
			m := manualSwapNode{node: ctx.Node, claim: ctx.Claim}
			found = &m
			return true
		})
		if err != nil {
			log.Printf("findLoaderForDemand: %v", err)
		}
		if found != nil {
			return found
		}
		e.debugFn("kanban: demand signal core_node=%s has no active loader for payload=%s — payload-first-match fallback",
			coreNodeName, payloadCode)
	}
	return e.FindLoaderForPayload(payloadCode)
}

// hasOptInLoaderThreshold returns true when a loader_payload_thresholds
// row exists for this (loader, payload) with replenish_uop_threshold > 0.
// Lookup failure returns false — better to over-fire L1 (which the
// countLoaderInFlightEmptyIn guard catches as a duplicate) than to leave
// a payload unstocked because a DB read flickered.
func (e *Engine) hasOptInLoaderThreshold(coreNodeName, payloadCode string) bool {
	row, err := e.db.GetLoaderPayloadThreshold(coreNodeName, payloadCode)
	if err != nil || row == nil {
		return false
	}
	return row.ReplenishUOPThreshold > 0
}

// isTransitionalLoader reports whether the loader at coreNodeName is in the
// transitional_loaders set — operator-driven, with the market-accounting L1
// paths (UOP-threshold C-push and legacy bin-count) suppressed in favour of
// opportunistic empty staging (MaybePushLoader) plus operator payload
// selection at the board.
//
// Fails OPEN (returns false = not transitional) on a DB read error, mirroring
// hasOptInLoaderThreshold: a transient flicker should let the automatic
// supply paths run rather than silently strand a loader the operator can't
// see is suppressed. (Treating an errored read as transitional would instead
// stop all supply until the DB recovers — the worse outcome.)
func (e *Engine) isTransitionalLoader(coreNodeName string) bool {
	if coreNodeName == "" {
		return false
	}
	on, err := e.db.IsTransitionalLoader(coreNodeName)
	if err != nil {
		e.logFn("transitional: lookup for %s failed, treating as non-transitional: %v", coreNodeName, err)
		return false
	}
	return on
}

// HandleLoopBelowThreshold is the Core→Edge LoopBelowThresholdSignal
// receiver. Operates in UOP space — the native unit of the threshold
// configuration — instead of going through refillLoaderForPayload's
// bin-count math:
//
//	projectedUOP := sig.CurrentUOP + inFlight * payload.UOPCapacity
//	needed       := ceil((threshold - projectedUOP) / capacity)
//
// projectedUOP is the asymptote of the current trajectory: each
// in-flight L1 will, once filled and returned via L2, contribute one
// bin's capacity of UOP. If that's already at or above threshold, the
// loop is on track and we skip. Otherwise we fire just enough L1s to
// close the gap, rounding up to whole bins.
//
// Distinct from the legacy DemandSignal path (MaybeCreateLoaderEmptyIn
// → refillLoaderForPayload), which keeps the magic-2 bin floor for
// loaders that haven't opted into UOP-threshold replenishment. The
// countLoaderInFlightEmptyIn guard is the dedup contract between the
// two paths.
//
// Capacity comes from payload_catalog.uop_capacity (synced from Core),
// not from claim.UOPCapacity — supermarket-side loaders carry
// UOPCapacity=0 since they don't consume parts themselves, while the
// payload's per-bin capacity is a property of the part. Missing or
// zero capacity is treated as a configuration error and skipped with
// a loud log; falling back to bin-count math would re-introduce the
// magic-floor over-fire this path exists to avoid.
//
// Reason carries either "below_threshold" or "warm_up_startup_sweep" —
// logged for diagnostics but behaves identically. Per-binding warm-up
// cap is enforced at Core; Edge just responds to each signal.
func (e *Engine) HandleLoopBelowThreshold(sig *protocol.LoopBelowThresholdSignal) {
	if sig == nil || sig.PayloadCode == "" {
		return
	}
	// Resolve the loader by the authoritative CoreNodeName the signal carries
	// (v6+), NOT by payload alone: when the same payload is loaded at two
	// loaders (multi-cell plants — the SNF2/SNF3 case this work centres on),
	// Core signals one binding, but a payload-only first-match can pick the
	// other and fire the L1 at the wrong window. FindLoaderClaimAt still walks
	// every style (Round-3 Obs 9: an INACTIVE-style loader must still receive
	// threshold-driven L1s). Fall back to payload-only resolution only for a
	// pre-v6 Core that didn't stamp CoreNodeName, logging the degraded path.
	var loader *manualSwapNode
	if sig.CoreNodeName != "" {
		loader = e.FindLoaderClaimAt(sig.CoreNodeName, sig.PayloadCode)
	} else {
		e.logFn("loop_threshold: signal for payload=%s has no core_node_name — payload-first-match fallback (pre-v6 Core?)", sig.PayloadCode)
		loader = e.FindAnyLoaderClaimForPayload(sig.PayloadCode)
	}
	if loader == nil {
		e.debugFn("loop_threshold: no loader for core_node=%s payload=%s — dropping signal", sig.CoreNodeName, sig.PayloadCode)
		return
	}
	e.debugFn("loop_threshold: signal received loader=%s payload=%s current=%d threshold=%d reason=%s",
		loader.node.CoreNodeName, sig.PayloadCode, sig.CurrentUOP, sig.Threshold, sig.Reason)

	entry, err := e.catalogService.GetByCode(sig.PayloadCode)
	if err != nil || entry == nil || entry.UOPCapacity <= 0 {
		e.logFn("loop_threshold: loader=%s payload=%s no per-bin capacity in catalog — skipping (err=%v)",
			loader.node.CoreNodeName, sig.PayloadCode, err)
		return
	}
	capacity := entry.UOPCapacity

	// Desired total in-flight bins to reach threshold from the CURRENT loop
	// UOP. tryCreateL1 subtracts what is already in flight and fires the
	// remainder — the in-flight dedup contract lives there now, not here.
	gap := sig.Threshold - sig.CurrentUOP
	if gap <= 0 {
		e.debugFn("loop_threshold: loader=%s payload=%s currentUOP=%d >= threshold=%d — skipping",
			loader.node.CoreNodeName, sig.PayloadCode, sig.CurrentUOP, sig.Threshold)
		return
	}
	desiredBins := (gap + capacity - 1) / capacity

	created, err := e.tryCreateL1(loader, sig.PayloadCode, L1LoopThreshold, desiredBins)
	if err != nil {
		e.logFn("loop_threshold: loader=%s payload=%s — L1 creation failed after %d created: %v",
			loader.node.CoreNodeName, sig.PayloadCode, created, err)
		return
	}
	if created > 0 {
		e.logFn("loop_threshold: loader=%s payload=%s firing %d L1 (currentUOP=%d threshold=%d capacity=%d)",
			loader.node.CoreNodeName, sig.PayloadCode, created, sig.CurrentUOP, sig.Threshold, capacity)
	}
}

// refillLoaderForPayload tops the per-payload empty-in queue at one loader
// up to (ReorderPoint - currentCount - inFlight) orders. Per-payload helper
// so MaybeCreateLoaderEmptyIn can sweep across all allowed payloads.
//
// LEGACY BIN-COUNT PATH ONLY. The UOP-threshold C-push path lives in
// HandleLoopBelowThreshold and does its own UOP-denominated math against
// payload_catalog.uop_capacity — do not route threshold-driven signals
// through here, the bin-count floor over-fires when threshold < capacity
// (one bin would satisfy a threshold of 100 UOP at capacity 345, but
// minStock=2 default would create two L1s).
//
// ReorderPoint semantics (produce-role): bin-count minimum-stock floor —
// "I want at least N bins of this payload in the kanban loop." currentCount
// is `systemBinCountForPayload`, which counts bins anywhere in the active
// lifecycle (at storage, in transit, staged at consumer lines, being
// filled at loaders) excluding flagged/maintenance/quality_hold/retired.
// The gate fires L1s only when total in-loop inventory drops below N.
// Zero ReorderPoint falls back to a magic-number floor of 2.
//
// Pre-2026-05-11 this used PreflightInventory's "available for sourcing"
// count, which excluded staged bins at non-storage nodes — so a bin
// staged at the consumer line didn't count toward inventory, and L1
// fired even when total system inventory was at the floor. SNF2 plant
// incident (76682-6TA0A.06, 2 bins in system, ReorderPoint=2, kept
// firing L1) was that drift.
//
// The future kanban calculator (shingo-kanban-calculator-design.md) writes
// its computed loop-size output into this same ReorderPoint column, so
// operator-set values today and calculator-driven values tomorrow share one
// read site.
//
// Fails OPEN on the system-count lookup: if Core can't be reached we treat
// currentCount as zero and top the queue up to ReorderPoint. Idle is worse
// than redundant.
func (e *Engine) refillLoaderForPayload(loader *manualSwapNode, payloadCode string) {
	// Pre-2026-05-12: a parked empty bin at the loader hard-blocked L1
	// creation across all payloads. The intent was to prevent a second
	// physical retrieve from wedging the floor (plant 2026-04-28 #483→#484:
	// Core dispatched a retrieve to a loader that already had its bin, then
	// later evicted the parked one). That gated the queue, not just the
	// dispatch — operators couldn't see incoming demand, and during a
	// changeover swap nothing fired at all (plant 2026-05-12).
	//
	// The dispatch-side safety net already exists at Core:
	// dispatch.CheckDropoffCapacity (capacity.go:86) blocks every retrieve
	// whose delivery node has an existing bin, putting the order in
	// `queued` status with a queue_reason. The fulfillment scanner
	// re-plans queued orders on every BinUpdatedEvent (wiring.go:228), so
	// when the parked bin clears, the queued L1 dispatches automatically.
	//
	// With that downstream gate proven, we let L1 creation proceed
	// freely: the operator HMI shows the queued demand, no robot moves
	// to the loader until there's room, no wedge.
	minStock := loader.claim.ReorderPoint
	if minStock <= 0 {
		minStock = 2
	}
	currentCount := 0
	if count, ok := e.systemBinCountForPayload(payloadCode); ok {
		currentCount = count
	}
	if currentCount >= minStock {
		e.logFn("side-cycle: loader %s — %d bins of %s in system (>=%d minimum), skipping L1",
			loader.node.Name, currentCount, payloadCode, minStock)
		return
	}
	// Desired total in-loop bins; tryCreateL1 subtracts what is already in
	// flight (fail-closed) and fires the remainder. The system-count
	// fail-OPEN above stays here at the caller — only the in-flight guard is
	// centralized.
	desired := minStock - currentCount
	created, err := e.tryCreateL1(loader, payloadCode, L1SideCycle, desired)
	if err != nil {
		e.logFn("side-cycle: loader %s payload %s — L1 creation failed after %d created: %v",
			loader.node.Name, payloadCode, created, err)
		return
	}
	if created > 0 {
		e.logFn("side-cycle: loader %s — created %d L1 retrieve_empty for %s (minStock=%d currentCount=%d)",
			loader.node.Name, created, payloadCode, minStock, currentCount)
	}
}

// L1Source identifies which path is creating a loader empty-in (L1)
// retrieve_empty order. It is the typed replacement for the old free-text
// `tag` and also carries the transitional-suppression policy, so adding a
// source forces a decision about its class rather than defaulting silently.
type L1Source string

const (
	L1SideCycle     L1Source = "side-cycle"     // legacy bin-count refill
	L1LoopThreshold L1Source = "loop_threshold" // UOP-threshold C-push
	L1LoaderPush    L1Source = "loader_push"    // transitional opportunistic empty staging
)

// logTag is the stable, greppable prefix this source uses in log lines.
func (s L1Source) logTag() string { return string(s) }

// suppressedByTransitional reports whether a transitional loader silences
// this source. Allowlist semantics: only the market-accounting (automatic)
// sources opt in. L1LoaderPush — the transitional supply path itself — and
// any future operator-driven source fall through to false, so they are NOT
// suppressed: the operator is the signal on a transitional loader, and the
// opportunistic empty staging that feeds them must keep running.
func (s L1Source) suppressedByTransitional() bool {
	switch s {
	case L1SideCycle, L1LoopThreshold:
		return true
	default:
		return false
	}
}

// tryCreateL1 is the single chokepoint for creating loader empty-in (L1)
// retrieve_empty orders. It owns the two gates and the fire loop; the
// source-specific "how many do we want" math stays at the caller and arrives
// as count — the desired total in-flight, NOT yet net of what is already in
// flight.
//
//  1. Transitional gate: if the loader is operator-driven (in the
//     transitional_loaders set) and source is an automatic market-accounting
//     path, fire nothing — the operator is the signal. Opportunistic empty
//     staging is not market-accounting and is not gated here.
//  2. In-flight dedup (fail CLOSED): count existing non-terminal
//     retrieve_empty orders for the payload and fire only count-inFlight. A
//     DB read error returns (0, err) — never fire into the dark. This is THE
//     dedup contract shared by the side-cycle and C-push paths (previously
//     two copy-paste guards); do not weaken it.
//
// Returns the number of L1s actually created. On a mid-loop create error it
// returns the count created so far plus the wrapped error; it does NOT roll
// back already-dispatched orders (they may be in flight at Core) — the next
// signal tops up the remainder via the same in-flight recount.
//
// All L1s use autoConfirm=false: the loader operator must confirm the bin is
// filled. Auto-confirming would immediately fire L2 and send the still-empty
// bin back to the supermarket (reproduced on plants with global auto-confirm,
// 2026-04-27). Source group is loader.claim.InboundSource so Core's
// planRetrieveEmpty pulls from the configured empty market, not a global FIFO
// scan.
func (e *Engine) tryCreateL1(loader *manualSwapNode, payload string, source L1Source, count int) (int, error) {
	coreNode := loader.node.CoreNodeName
	if e.isTransitionalLoader(coreNode) && source.suppressedByTransitional() {
		e.debugFn("%s: loader=%s payload=%s skipped — transitional, operator-driven",
			source.logTag(), coreNode, payload)
		return 0, nil
	}
	inFlight, err := e.countLoaderInFlightEmptyIn(loader.node.CoreNodeName, payload)
	if err != nil {
		// Fail closed — do not fire into the dark; the next signal retries.
		e.logFn("%s: loader=%s payload=%s in-flight count lookup failed — skipping: %v",
			source.logTag(), coreNode, payload, err)
		return 0, err
	}
	toFire := count - inFlight
	if toFire <= 0 {
		e.debugFn("%s: loader=%s payload=%s already has %d in-flight >= %d wanted — skipping",
			source.logTag(), coreNode, payload, inFlight, count)
		return 0, nil
	}
	nodeID := loader.node.ID
	created := 0
	for i := 0; i < toFire; i++ {
		order, err := e.orderMgr.CreateRetrieveOrder(
			&nodeID, true, 1, coreNode, loader.claim.InboundSource, "",
			"standard", payload, false, true,
		)
		if err != nil {
			return created, fmt.Errorf("%s: create L1 %d/%d loader=%s payload=%s: %w",
				source.logTag(), i+1, toFire, coreNode, payload, err)
		}
		created++
		e.debugFn("%s: L1 order %d (%d/%d) loader=%s payload=%s",
			source.logTag(), order.ID, i+1, toFire, coreNode, payload)
	}
	return created, nil
}

// loaderInFlightEmptyCount counts non-terminal retrieve_empty orders inbound to
// the loader's CORE NODE regardless of payload tag. MaybePushLoader uses it to
// keep exactly one empty staged; countLoaderInFlightEmptyIn is the per-payload
// variant the threshold/legacy paths use. Keyed by core node (delivery_node) so
// a shared loader's sibling process_node rows don't each under-count and stage
// duplicate empties into one slot; see [[shingo_manual_swap_core_node_scoping]].
func (e *Engine) loaderInFlightEmptyCount(coreNodeName string) (int, error) {
	n, err := e.countActiveOrdersAtNode(coreNodeName, func(o orders.Order) bool {
		return o.RetrieveEmpty
	})
	if err != nil {
		return 0, fmt.Errorf("list active orders for node %s: %w", coreNodeName, err)
	}
	return n, nil
}

// MaybePushLoader is the loader-side mirror of MaybePushUnloader: the
// opportunistic empty-staging push for TRANSITIONAL loaders. When a
// transitional loader's window is free it stages one empty so the operator
// always has a bin to fill. Non-transitional loaders are no-ops here — their
// empties come from the threshold/legacy paths (which know the payload and
// count). Opportunistic, one at a time: maybeStageLoaderEmpty fires only when
// no empty is already in flight, and Core's CheckDropoffCapacity queues the
// order if the window is still physically occupied, so it can't slam.
//
// Trigger sites mirror the unloader:
//   - applyManualSwap (L2 arrived at the market — window confirmed free).
//   - ClearBin (operator cleared the window).
//   - SweepPushLoaders on Edge startup / registration ack.
//
// nodeID names a specific loader (typically the one whose window just freed);
// pass 0 for an "any transitional loader" sweep — see SweepPushLoaders.
func (e *Engine) MaybePushLoader(nodeID int64) {
	for _, m := range e.findManualSwapNodes("") {
		if nodeID != 0 && m.node.ID != nodeID {
			continue
		}
		if m.claim.Role != protocol.ClaimRoleProduce {
			continue
		}
		if !e.isTransitionalLoader(m.node.CoreNodeName) {
			continue
		}
		e.maybeStageLoaderEmpty(m)
	}
}

// maybeStageLoaderEmpty stages one empty at a transitional loader if none is
// already in flight. The empty is a generic carrier staged payload-AGNOSTIC
// (blank code) rather than tagged with an arbitrary "representative" payload —
// there is no payload-specific demand behind an opportunistic stage, so naming
// one just fabricates a binding the operator routinely overrides at LoadBin.
// One-at-a-time keeps it opportunistic; L1LoaderPush is exempt from the
// transitional suppression in tryCreateL1 (it IS the transitional supply path).
//
// Single-carrier assumption — see RequestEmptyBin: a blank order sources any
// compatible empty, which is correct only when the loader uses one carrier type.
func (e *Engine) maybeStageLoaderEmpty(loader manualSwapNode) {
	// Misconfig guard stays: a loader with no allowed payloads isn't set up to
	// load anything, so there's nothing to stage for — even agnostically.
	if len(loader.claim.AllowedPayloads()) == 0 {
		return // misconfigured loader — nothing to stage against
	}
	inFlight, err := e.loaderInFlightEmptyCount(loader.node.CoreNodeName)
	if err != nil {
		e.logFn("loader-push: in-flight lookup at %s failed — skipping: %v", loader.node.CoreNodeName, err)
		return
	}
	if inFlight > 0 {
		e.debugFn("loader-push: %s already has %d empty in flight — skipping", loader.node.CoreNodeName, inFlight)
		return
	}
	if _, err := e.tryCreateL1(&loader, "", L1LoaderPush, 1); err != nil {
		e.logFn("loader-push: stage empty at %s failed: %v", loader.node.CoreNodeName, err)
	}
}

// SweepPushLoaders walks every active transitional produce manual_swap loader
// and stages an empty if its window is free. Intended for Edge startup (after
// registration ack, mirroring SweepPushUnloaders): catches loaders that were
// empty when Edge went down so the operator returns to a staged empty rather
// than an empty window.
func (e *Engine) SweepPushLoaders() {
	if !e.sweepingLoaders.CompareAndSwap(false, true) {
		return // a sweep is already running — a re-register storm must not stack them
	}
	defer e.sweepingLoaders.Store(false)
	matches := e.findManualSwapNodes("")
	swept := 0
	for _, m := range matches {
		if m.claim.Role != protocol.ClaimRoleProduce {
			continue
		}
		if !e.isTransitionalLoader(m.node.CoreNodeName) {
			continue
		}
		e.maybeStageLoaderEmpty(m)
		swept++
	}
	if swept > 0 {
		log.Printf("loader-push: startup sweep covered %d transitional loader(s)", swept)
	}
}
