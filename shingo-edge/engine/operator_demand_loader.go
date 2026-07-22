package engine

import (
	"fmt"
	"log"
	"sync"

	"shingo/protocol"
	"shingoedge/domain"
)

// manualSwapWindowSlots is how many bins a single manual_swap core node can
// physically stage at its window — one (one physical slot per window/position).
//
// The LOADER empty path no longer reads this constant: reserveLoaderEmpties
// derives the budget from the delivery-node SET cardinality (one bin per node),
// so a multi-window loader's budget grows to N when delivery spreads (C4+)
// without a magic number, and the per-payload dedup + capacity cap are unified in
// the seam. The constant remains for the UNLOADER full-in cap
// (operator_demand_unloader.go), which has not yet moved to a reservation seam,
// and still documents the one-bin-per-node physical model the operator-path
// anti-spam guard also encodes (operator_bin_ops.go).
const manualSwapWindowSlots = 1

// The legacy bin-count produce DemandSignal trigger (MaybeCreateLoaderEmptyIn +
// findLoaderForDemand + refillLoaderForPayload) is RETIRED. A produce loader is now
// either operator-driven (window-free opportunistic staging — MaybePushLoader) or
// threshold-driven (UOP kanban autoreorder — HandleLoopBelowThreshold); there is no
// bin-count floor. Core still emits produce DemandSignals on bin movements, but the
// Edge no longer routes them to a handler (see cmd/shingoedge/main.go) — supply comes
// from the threshold monitor and the operator push, both via the reserveLoaderBins seam.

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
// reserveLoaderEmpties seam is the dedup contract between the
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
	// C3: resolve through the LoaderStore (the flag dual). LoaderAt covers every
	// style (the aggregate is styleless; the legacy walk is all-styles) so an
	// INACTIVE-style loader still receives threshold L1s (Round-3 Obs 9). On a
	// store error or a loader that doesn't serve the payload, drop the signal —
	// never reroute. Fall back to payload-first-match only for a pre-v6 Core that
	// didn't stamp CoreNodeName.
	pay := domain.PayloadCode(sig.PayloadCode)
	var loader *domain.Loader
	// Resolve by the loader IDENTITY token first (step-4 cutover): a synthetic loader
	// with no anchor node resolves here where LoaderAt(CoreNodeName) cannot. Fall back
	// to the binding node (pre-cutover / legacy Core), then payload-first-match (pre-v6
	// Core that stamped no node at all).
	if sig.LoaderKey != "" {
		if l, err := e.loaders().LoaderByKey(domain.LoaderID(sig.LoaderKey), domain.RoleProduce); err == nil && l != nil && l.ServesPayload(pay) {
			loader = l
		}
	}
	if loader == nil && sig.CoreNodeName != "" {
		if l, err := e.loaders().LoaderAt(domain.NodeID(sig.CoreNodeName), domain.RoleProduce); err == nil && l != nil && l.ServesPayload(pay) {
			loader = l
		}
	}
	if loader == nil && sig.LoaderKey == "" && sig.CoreNodeName == "" {
		e.logFn("loop_threshold: signal for payload=%s has no loader_key/core_node_name — payload-first-match fallback (pre-v6 Core?)", sig.PayloadCode)
		if l, err := e.loaders().LoaderForPayload(pay, domain.RoleProduce, false); err == nil {
			loader = l
		}
	}
	if loader == nil {
		// Startup race: if the loader cache hasn't synced yet, this miss is almost
		// certainly the signal beating the node-list sync — park it for replay rather
		// than drop it (closes the gap where a fresh-restart reorder was lost until
		// the next delta). After the cache has warmed once, a miss is genuine.
		if e.parkThresholdSignalIfCold(sig) {
			return
		}
		e.debugFn("loop_threshold: no loader for core_node=%s payload=%s — dropping signal", sig.CoreNodeName, sig.PayloadCode)
		return
	}
	e.debugFn("loop_threshold: signal received loader=%s payload=%s current=%d threshold=%d reason=%s",
		loader.ID(), sig.PayloadCode, sig.CurrentUOP, sig.Threshold, sig.Reason)

	entry, err := e.catalogService.GetByCode(sig.PayloadCode)
	if err != nil || entry == nil || entry.UOPCapacity <= 0 {
		e.logFn("loop_threshold: loader=%s payload=%s no per-bin capacity in catalog — skipping (err=%v)",
			loader.ID(), sig.PayloadCode, err)
		return
	}
	capacity := entry.UOPCapacity

	// Desired total in-flight bins to reach threshold from the CURRENT loop
	// UOP. tryCreateL1 subtracts what is already in flight and fires the
	// remainder — the in-flight dedup contract lives there now, not here.
	gap := sig.Threshold - sig.CurrentUOP
	if gap <= 0 {
		e.debugFn("loop_threshold: loader=%s payload=%s currentUOP=%d >= threshold=%d — skipping",
			loader.ID(), sig.PayloadCode, sig.CurrentUOP, sig.Threshold)
		return
	}
	desiredBins := (gap + capacity - 1) / capacity
	e.debugFn("loop_threshold: loader=%s payload=%s gap=%d capacity=%d desired_bins=%d",
		loader.ID(), sig.PayloadCode, gap, capacity, desiredBins)

	// Route the empty to the member the signal names (the same-payload-two-positions
	// fix). Pre-step-4 the member rides MemberNodeName; fall back to CoreNodeName,
	// which still doubles as the member until the identity cutover splits them.
	member := domain.NodeID(sig.MemberNodeName)
	if member == "" {
		member = domain.NodeID(sig.CoreNodeName)
	}
	created, err := e.tryCreateL1(loader, pay, L1LoopThreshold, desiredBins, member)
	if err != nil {
		e.logFn("loop_threshold: loader=%s payload=%s — L1 creation failed after %d created: %v",
			loader.ID(), sig.PayloadCode, created, err)
		return
	}
	if created > 0 {
		e.logFn("loop_threshold: loader=%s payload=%s firing %d L1 (currentUOP=%d threshold=%d capacity=%d)",
			loader.ID(), sig.PayloadCode, created, sig.CurrentUOP, sig.Threshold, capacity)
	}
}

// parkThresholdSignalIfCold parks a LoopBelowThresholdSignal that could not resolve
// a loader because the loader cache has not synced yet — the fresh-restart race
// where the signal beats the node-list sync. Returns true if parked (caller returns
// without dropping). Once the cache has warmed once, returns false so a genuine miss
// is still dropped (and self-heals via the next delta) rather than parked forever.
func (e *Engine) parkThresholdSignalIfCold(sig *protocol.LoopBelowThresholdSignal) bool {
	if e.loaderCacheWarmed.Load() {
		return false
	}
	e.pendingThreshMu.Lock()
	defer e.pendingThreshMu.Unlock()
	if e.loaderCacheWarmed.Load() { // a sync may have warmed it between the check and the lock
		return false
	}
	e.pendingThreshold = append(e.pendingThreshold, sig)
	e.logFn("loop_threshold: parked signal core_node=%s payload=%s — loader cache not synced yet (will replay on sync)",
		sig.CoreNodeName, sig.PayloadCode)
	return true
}

// warmLoaderCacheAndReplay marks the loader cache warmed on its first sync and
// replays every threshold signal parked before then. Idempotent: the replay runs
// exactly once, on the first SetCoreLoaders. Replayed signals re-enter
// HandleLoopBelowThreshold, which now resolves them against the freshly-synced cache.
func (e *Engine) warmLoaderCacheAndReplay() {
	e.pendingThreshMu.Lock()
	if e.loaderCacheWarmed.Load() {
		e.pendingThreshMu.Unlock()
		return
	}
	e.loaderCacheWarmed.Store(true)
	parked := e.pendingThreshold
	e.pendingThreshold = nil
	e.pendingThreshMu.Unlock()

	for _, sig := range parked {
		e.logFn("loop_threshold: replaying parked signal core_node=%s payload=%s after loader-cache sync",
			sig.CoreNodeName, sig.PayloadCode)
		e.HandleLoopBelowThreshold(sig)
	}
}

// L1Source identifies which path is creating a loader empty-in (L1)
// retrieve_empty order. The legacy bin-count source (L1SideCycle) is retired —
// a loader is supplied by the UOP-threshold C-push (L1LoopThreshold) or the
// operator-driven opportunistic push (L1LoaderPush). It also carries the
// operator-driven-suppression policy, so adding a source forces a decision
// about its class rather than defaulting silently.
type L1Source string

const (
	L1LoopThreshold L1Source = "loop_threshold" // UOP-threshold C-push
	L1LoaderPush    L1Source = "loader_push"    // operator-driven opportunistic empty staging
)

// logTag is the stable, greppable prefix this source uses in log lines.
func (s L1Source) logTag() string { return string(s) }

// suppressedByOperatorDriven reports whether an operator-driven loader silences
// this source. Allowlist semantics: only the automatic market-accounting source
// (L1LoopThreshold) opts in — an operator-driven loader is fed by the operator,
// not the threshold monitor. L1LoaderPush (the operator-driven supply path itself)
// falls through to false, so it is NOT suppressed.
func (s L1Source) suppressedByOperatorDriven() bool {
	return s == L1LoopThreshold
}

// loaderResvLock returns the per-loader reservation mutex, creating it on first
// use. Keyed by loader id (the resolved core node in C1) so two loaders never
// block each other — a slow burst on loader X can't stall loader Y.
func (e *Engine) loaderResvLock(loaderID string) *sync.Mutex {
	m, _ := e.loaderResv.LoadOrStore(loaderID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// reserveLoaderEmpties is THE chokepoint that makes count→fire atomic for a
// loader. Under the loader's mutex it counts non-terminal retrieve_empty orders
// across the delivery-node set in ONE snapshot, applies the per-payload dedup and
// the loader-capacity cap, and fires the remainder via the caller's `fire`
// closure — all without releasing the lock, so a concurrent demand signal or
// operator request cannot interleave between the count and the create. This is
// the never-2N guarantee, and EVERY empty-firing writer routes through here
// (tryCreateL1 for the threshold/side-cycle paths; RequestEmptyBin for the
// operator path; maybeStageLoaderEmpty/MaybePushLoader via tryCreateL1).
//
// want is the desired TOTAL in-flight for this payload; toFire = want minus what
// is already in flight for the payload, capped to the loader's free capacity
// (budget = one bin per delivery node, minus all in-flight empties across the set).
//
// NO transaction, by design. The only operation that RAISES a loader's empty
// count is the create inside `fire`; every other mutation (completion,
// cancellation, failure) only lowers it, so serialising the up-writers with the
// mutex makes the count monotone-safe without DB isolation. And
// CreateRetrieveOrder is not transaction-pure — it enqueues to Core and fires a
// synchronous EmitOrderCreated mid-write — so a surrounding tx could only
// manufacture the Core/Edge divergence it was meant to prevent. See
// FINAL-ADJUDICATION Q1 (monotonicity + unsoundness arguments).
//
// Fails CLOSED: a count read error fires nothing; the next signal retries.
//
// RE-ENTRANCY RULE (pinned, do not assume — it is enforced by a test): `fire`
// runs while the loader's mutex is held and calls CreateRetrieveOrder, which
// fires EmitOrderCreated SYNCHRONOUSLY on the in-process event bus. No
// order-event subscriber may call back into the reservation seam for the SAME
// loader — sync.Mutex is non-reentrant and would self-deadlock. If a subscriber
// ever needs to re-enter, split reserve-from-fire (end the lock after the DB
// insert; enqueue/emit after release). TestReserveLoaderEmpties_EmitDuringReservation
// guards that the live subscribers do not re-enter.
// reserveLoaderBins is the single never-2N chokepoint for BOTH directions: a loader's
// empty-in (retrieveEmpty=true) and an unloader's full-in (retrieveEmpty=false). It was
// reserveLoaderEmpties (empty-only); the body is role-agnostic except the in-flight
// filter, so the consume side now shares it instead of re-implementing the count/cap
// (the loader/unloader drift). retrieveEmpty selects which in-flight orders the budget
// counts; the caller's fire closure creates the matching order type.
func (e *Engine) reserveLoaderBins(loader *domain.Loader, payload domain.PayloadCode, want int, member domain.NodeID, retrieveEmpty bool, fire func(deliveryNodes []string) (int, error)) (int, error) {
	if loader == nil || want <= 0 {
		return 0, nil
	}
	// The Loader owns the reservation shape: which nodes the count spans and the
	// budget. multiWindowEnabled gates whether a shared loader spreads across its
	// windows (budget = SlotCount) or funnels to its anchor (budget 1); member
	// routes a dedicated reservation to the position the signal named (O2 fix) —
	// see domain.Loader.ReservationTarget.
	nodes, budget := loader.ReservationTarget(member, payload, e.multiWindowEnabled())
	if len(nodes) == 0 || budget <= 0 {
		return 0, nil // loader doesn't serve this payload — no target
	}
	deliveryNodes := nodeIDStrings(nodes)
	loaderID := string(loader.ID())
	pay := string(payload)

	mu := e.loaderResvLock(loaderID)
	mu.Lock()
	defer mu.Unlock()

	orderList, err := e.db.ListActiveOrdersByDeliveryNodeSet(deliveryNodes)
	if err != nil {
		// Fail closed — never fire into the dark when the order list is unavailable.
		return 0, fmt.Errorf("reserve loader=%s: in-flight count: %w", loaderID, err)
	}
	inFlightPayload, inFlightTotal := 0, 0
	perNode := make(map[string]int, len(deliveryNodes))
	for _, o := range orderList {
		if o.RetrieveEmpty != retrieveEmpty {
			continue // count only this direction's in-flight (empties for a loader, fulls for an unloader)
		}
		inFlightTotal++
		perNode[o.DeliveryNode]++
		if o.PayloadCode == pay {
			inFlightPayload++
		}
	}
	toFire := want - inFlightPayload
	if headroom := budget - inFlightTotal; toFire > headroom {
		toFire = headroom
	}
	if toFire <= 0 {
		e.logFn("loader_reserve loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d budget=%d to_fire=0 created=0",
			loaderID, pay, want, inFlightPayload, inFlightTotal, budget)
		return 0, nil
	}
	// Assign each new empty to a FREE window (none in flight) — one physical bin
	// per window. budget = window count and toFire ≤ headroom = the free-window
	// count, so there are always enough; a single-node set degrades to [that node].
	targets := make([]string, 0, toFire)
	for _, node := range deliveryNodes {
		if len(targets) >= toFire {
			break
		}
		if perNode[node] == 0 {
			targets = append(targets, node)
		}
	}
	created, ferr := fire(targets)
	// Structured decision record — one machine-parseable line per reservation so an
	// over-ordering incident is reconstructable from logs alone (the SLN_002 bar).
	e.logFn("loader_reserve loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d budget=%d to_fire=%d targets=%v created=%d err=%v",
		loaderID, pay, want, inFlightPayload, inFlightTotal, budget, toFire, targets, created, ferr)
	return created, ferr
}

// multiWindowEnabled reports whether shared-window multi-window delivery is on
// (config LoadersMultiWindow). DEFAULT ON (nil cfg/flag = enabled): a shared
// loader spreads empties across its windows; set `loaders_multi_window: false`
// to funnel to the anchor with budget 1 instead.
func (e *Engine) multiWindowEnabled() bool {
	return e.cfg == nil || e.cfg.LoadersMultiWindow == nil || *e.cfg.LoadersMultiWindow
}

// nodeIDStrings projects typed NodeIDs to the plain strings the order-query layer
// keys on (the boundary where typed IDs meet the legacy string columns).
func nodeIDStrings(ns []domain.NodeID) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = string(n)
	}
	return out
}

// loaderEmptySource is the group an L1 empty is RETRIEVED FROM. A loader with a
// configured buffer (the near-line staging group, step 7) sources from it, so empties
// rotate buffer→position to satisfy a threshold fill; the buffer is kept stocked by the
// cell routing its emptied carriers back into it (plant config). Falls back to the
// far-upstream inbound_source only when no buffer is CONFIGURED.
//
// TODO(prod): EVALUATE FALLBACK-WHEN-DRY AGAINST REAL-PLANT BEHAVIOR. Today a buffered
// loader sources UNCONDITIONALLY from the buffer — if the buffer is momentarily empty the
// L1 just queues until the downstream cell recycles an empty back into it. That's fine for
// the dev sim (a closed buffer↔cell loop), but in a real plant a slow or stalled
// downstream cell would STARVE the loader, since it never reaches past the buffer. The
// production-correct rule is almost certainly buffer-FIRST with a FALLBACK to
// inbound_source (the big return bank) when the buffer is dry: the buffer as a near-line
// cache, the return bank as the never-empty backstop, so the loader never idles. That
// needs a runtime "does the buffer group hold an unclaimed empty?" check, which the Edge
// can't do today — FetchNodeBins is per-NODE, not per-group, and there is no
// empties-in-group query; the Edge only knows the buffer's group NAME, not its member
// slots. Wiring it means a small Core endpoint (or threading the buffer's slots onto the
// aggregate) plus a per-L1 lookup (mind the latency). Decide the real-plant semantics —
// and whether a per-L1 Core round-trip is acceptable — before building it.
func loaderEmptySource(l *domain.Loader) string {
	if b := l.BufferDest(); b != "" {
		return b
	}
	return l.InboundSource()
}

// tryCreateL1 is the threshold/side-cycle entry to the reservation seam. It takes
// the resolved *domain.Loader (C3: the Loader is the unit of resolution, not the
// old manualSwapNode shim). The operator-driven gate is applied here; the count→fire
// atomicity, the per-payload dedup, the capacity cap, and the decision record all
// live in reserveLoaderBins. count is the desired total in-flight for the payload.
func (e *Engine) tryCreateL1(loader *domain.Loader, payload domain.PayloadCode, source L1Source, count int, member domain.NodeID) (int, error) {
	if loader == nil {
		return 0, nil
	}
	coreNode := string(loader.ID())
	// IsOperatorDriven reads the aggregate directly — correct after loader.ID() became
	// the loader_key token (a cache lookup keyed on the old core_node_name would now
	// miss). The push source is exempt regardless (it IS the operator-driven supply path).
	if loader.IsOperatorDriven() && source.suppressedByOperatorDriven() {
		e.debugFn("%s: loader=%s payload=%s skipped — operator-driven",
			source.logTag(), coreNode, payload)
		return 0, nil
	}
	if loaderEmptySource(loader) == "" {
		// No inbound/buffer source to pull empties from — a forklift/press-fed loader is
		// supplied directly (operator stages empties at the window). Skip auto-L1; nothing
		// to queue. Symmetric to the unloader's no-inbound gate in createUnloaderFullInViaSeam.
		e.debugFn("%s: loader=%s payload=%s skipped — no inbound source (fed directly)",
			source.logTag(), coreNode, payload)
		return 0, nil
	}
	created, err := e.reserveLoaderBins(loader, payload, count, member, true, func(deliveryNodes []string) (int, error) {
		made := 0
		for i, deliveryNode := range deliveryNodes {
			node, nerr := e.db.GetProcessNodeByCoreNodeName(deliveryNode)
			if nerr != nil || node == nil {
				return made, fmt.Errorf("%s: no process_node for delivery target %s: %w", source.logTag(), deliveryNode, nerr)
			}
			nodeID := node.ID
			order, cerr := e.orderMgr.CreateRetrieveOrder(
				&nodeID, true, 1, deliveryNode, loaderEmptySource(loader), "",
				"standard", string(payload), false, true,
			)
			if cerr != nil {
				return made, fmt.Errorf("%s: create L1 %d/%d loader=%s payload=%s: %w",
					source.logTag(), i+1, len(deliveryNodes), coreNode, payload, cerr)
			}
			made++
			// Burst tripwire stays DELIVERY-NODE-keyed (orthogonal to loader identity):
			// one empty per physical window, so a flood at a single node trips it even
			// when the loader identity is now an opaque token.
			e.recordL1Burst(deliveryNode, 1)
			e.debugFn("%s: L1 order %d (%d/%d) loader=%s payload=%s window=%s",
				source.logTag(), order.ID, i+1, len(deliveryNodes), coreNode, payload, deliveryNode)
		}
		return made, nil
	})
	if err != nil {
		e.logFn("%s: loader=%s payload=%s reservation failed after %d created: %v",
			source.logTag(), coreNode, payload, created, err)
		return created, err
	}
	return created, nil
}

// MaybePushLoader is the loader-side mirror of MaybePushUnloader: the
// opportunistic empty-staging push for OPERATOR-DRIVEN loaders. When an
// operator-driven loader's window is free it stages one empty so the operator
// always has a bin to fill. Threshold loaders are no-ops here — their
// empties come from the threshold path (which knows the payload and
// count). Opportunistic, one at a time: maybeStageLoaderEmpty fires only when
// no empty is already in flight, and Core's CheckDropoffCapacity queues the
// order if the window is still physically occupied, so it can't slam.
//
// Trigger sites mirror the unloader:
//   - applyManualSwap (L2 arrived at the market — window confirmed free).
//   - ClearBin (operator cleared the window).
//   - SweepPushLoaders on Edge startup / registration ack.
//
// The nodeID arg is now vestigial — the reservation seam's never-2N budget makes
// the sweep idempotent (already-staged loaders create nothing), so there is no need
// to filter to a specific loader. Mirrors MaybePushUnloader(_ int64).
func (e *Engine) MaybePushLoader(_ int64) {
	loaders, err := e.loaders().Loaders(domain.RoleProduce)
	if err != nil {
		e.logFn("loader-push: list produce loaders: %v", err)
		return
	}
	for _, l := range loaders {
		// UsesOperatorStaging is true for operator-driven loaders AND for a
		// threshold loader with no threshold configured (the fallback — it would
		// otherwise be silently starved). SweepPushLoaders logs that misconfig.
		if !l.UsesOperatorStaging() {
			continue
		}
		e.maybeStageLoaderEmpty(l)
	}
}

// maybeStageLoaderEmpty stages one empty at an operator-driven loader if none is
// already in flight. The empty is a generic carrier staged payload-AGNOSTIC
// (blank code) rather than tagged with an arbitrary "representative" payload —
// there is no payload-specific demand behind an opportunistic stage, so naming
// one just fabricates a binding the operator routinely overrides at LoadBin.
// One-at-a-time keeps it opportunistic; L1LoaderPush is exempt from the
// operator-driven suppression in tryCreateL1 (it IS the operator-driven supply path).
//
// Single-carrier assumption — see RequestEmptyBin: a blank order sources any
// compatible empty, which is correct only when the loader uses one carrier type.
func (e *Engine) maybeStageLoaderEmpty(loader *domain.Loader) {
	if loader == nil {
		return
	}
	// Misconfig guard: a loader with nothing to stage against (no shared payloads
	// and no positions) isn't set up to load anything, so there's nothing to stage
	// for — even agnostically.
	if len(loader.PayloadSet()) == 0 && len(loader.Positions()) == 0 {
		return // misconfigured loader — nothing to stage against
	}
	// No separate advisory in-flight pre-check: the reservation seam owns the
	// never-2N dedup atomically across the loader's delivery nodes, so a push for a
	// loader that already has an empty in flight resolves to to_fire=0 and fires
	// nothing. The empty is staged payload-AGNOSTIC (blank code) — the operator
	// picks the payload at LoadBin; L1LoaderPush is exempt from the operator-driven
	// suppression in tryCreateL1 (it IS the operator-driven supply path).
	if _, err := e.tryCreateL1(loader, "", L1LoaderPush, 1, ""); err != nil { // opportunistic push: payload-agnostic, no member
		e.logFn("loader-push: stage empty at loader=%s failed: %v", loader.ID(), err)
	}
}

// SweepPushLoaders walks every active operator-driven produce manual_swap loader
// and stages an empty if its window is free. Intended for Edge startup (after
// registration ack, mirroring SweepPushUnloaders): catches loaders that were
// empty when Edge went down so the operator returns to a staged empty rather
// than an empty window.
func (e *Engine) SweepPushLoaders() {
	if !e.sweepingLoaders.CompareAndSwap(false, true) {
		return // a sweep is already running — a re-register storm must not stack them
	}
	defer e.sweepingLoaders.Store(false)
	loaders, err := e.loaders().Loaders(domain.RoleProduce)
	if err != nil {
		e.logFn("loader-push: startup sweep list produce loaders: %v", err)
		return
	}
	swept := 0
	for _, l := range loaders {
		if !l.UsesOperatorStaging() {
			continue
		}
		if l.MisconfiguredThreshold() {
			// Visible error: the loader is set to threshold replenishment but no UOP
			// threshold is configured, so Core never signals it — it falls back to
			// operator staging here. Fix the threshold in the loader config. (The
			// loaders-admin UI surfaces the same misconfig flag.)
			log.Printf("WARN loader-push: loader=%s replenishment=threshold but NO threshold configured — falling back to operator staging (set a UOP threshold in the loader config)", l.ID())
		}
		e.maybeStageLoaderEmpty(l)
		swept++
	}
	if swept > 0 {
		log.Printf("loader-push: startup sweep covered %d operator-staged loader(s)", swept)
	}
}
