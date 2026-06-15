package engine

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/orders"
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
	loader, viaFallback := e.findLoaderForDemand(coreNodeName, payloadCode)
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
	// instead. The reserveLoaderEmpties seam on both paths is
	// the dedup contract for the race window where both signals arrive
	// near-simultaneously — do not remove or weaken either guard.
	//
	// Fallback-no-sweep (PR-0): when the loader was resolved via the
	// payload-first-match fallback — the signal named no node, or named a node
	// with no matching active loader — refill ONLY the signaled payload, never
	// the resolved loader's whole AllowedPayloads catalog. The fallback is the
	// NORMAL kanban path (R1), so a full-catalog sweep here fans an unrelated
	// signal across every payload the resolved loader happens to list: the
	// Springfield SLN_002 Factor C amplifier (one SMN_001 signal resolved by
	// payload to SLN_002, then swept ~20 payloads → ~40 orders at a one-bin node).
	// A shared_window loader sweeps its whole allowed set (multi-payload top-up in
	// one signal). A dedicated loader has an empty shared set — each position is
	// independent — so it (and any fallback-resolved loader) refills ONLY the
	// signaled payload, never a full-catalog fan-out (the SLN_002 Factor C fix).
	codes := loader.PayloadSet()
	if viaFallback || len(codes) == 0 {
		codes = []domain.PayloadCode{domain.PayloadCode(payloadCode)}
	}
	created := 0
	for _, code := range codes {
		if e.hasOptInLoaderThreshold(string(loader.ID()), string(code)) {
			e.debugFn("kanban: HandleDemandSignal skip loader=%s payload=%s — C-push active",
				loader.ID(), code)
			continue
		}
		created += e.refillLoaderForPayload(loader, code)
	}
	// Sweep-summary observability (PR-0): one line per demand signal so a burst
	// is visible at a glance — the per-payload :321 lines were the only reason
	// the SLN_002 incident was diagnosable after the fact.
	e.debugFn("side-cycle: loader=%s sweep_complete created=%d payloads_considered=%d via_fallback=%v signal_payload=%s",
		loader.ID(), created, len(codes), viaFallback, payloadCode)
}

// findLoaderForDemand resolves the active-style produce loader for a legacy
// DemandSignal. It prefers the node Core named (coreNodeName) so a payload
// loaded at two separate loaders routes to the one the signal is about — the
// same protection HandleLoopBelowThreshold has on the threshold path. It
// falls back to payload-first-match when the signal carries no node, OR the
// named node has no matching active loader, so the most load-bearing path
// keeps working even if Core's DemandSignal node semantics differ from the
// assumption (fail-safe pending SME confirmation of those semantics).
//
// Returns (loader, viaFallback): viaFallback is true when resolution went
// through the payload-first-match fallback rather than the exact named node.
// The caller uses it to scope the refill — a fallback-resolved loader must
// refill ONLY the signaled payload, not its whole catalog (PR-0 Factor C fix).
//
// C3: resolves through the LoaderStore (the flag-selected dual) and returns a
// *domain.Loader. Fails CLOSED on a real store error — a transient DB flicker
// must drop the signal, not reroute it via payload-first-match to the wrong
// loader (the F7 bug). A clean miss (ErrLoaderNotFound) is the legitimate
// fallback trigger.
func (e *Engine) findLoaderForDemand(coreNodeName, payloadCode string) (*domain.Loader, bool) {
	role := domain.RoleProduce
	pay := domain.PayloadCode(payloadCode)
	if coreNodeName != "" {
		l, err := e.loaders().LoaderAt(domain.NodeID(coreNodeName), role)
		switch {
		case err == nil && l != nil && l.ServesPayload(pay):
			return l, false
		case err != nil && !errors.Is(err, ErrLoaderNotFound):
			e.logFn("kanban: demand signal core_node=%s loader-store error — failing closed: %v", coreNodeName, err)
			return nil, false
		default:
			e.debugFn("kanban: demand signal core_node=%s has no loader for payload=%s — payload-first-match fallback",
				coreNodeName, payloadCode)
		}
	}
	l, err := e.loaders().LoaderForPayload(pay, role, true)
	if err != nil || l == nil {
		return nil, true
	}
	return l, true
}

// hasOptInLoaderThreshold returns true when Core's loader aggregate carries a
// replenish UOP threshold > 0 for this (loader, payload) — i.e. the loader opted
// into UOP-threshold (C-push) replenishment for that payload. Lookup failure
// returns false: fall back to the bin-count L1 path rather than strand a payload
// because a cache read flickered.
func (e *Engine) hasOptInLoaderThreshold(coreNodeName, payloadCode string) bool {
	return e.hasThresholdFromCore(coreNodeName, payloadCode)
}

// isTransitionalLoader reports whether the loader at coreNodeName is in the
// transitional_loaders set — operator-driven, with the market-accounting L1
// paths (UOP-threshold C-push and legacy bin-count) suppressed in favour of
// opportunistic empty staging (MaybePushLoader) plus operator payload
// selection at the board.
//
// Fails OPEN (returns false = not transitional) on a cache read error, mirroring
// hasOptInLoaderThreshold: a transient flicker should let the automatic
// supply paths run rather than silently strand a loader the operator can't
// see is suppressed. (Treating an errored read as transitional would instead
// stop all supply until the read recovers — the worse outcome.)
func (e *Engine) isTransitionalLoader(coreNodeName string) bool {
	if coreNodeName == "" {
		return false
	}
	return e.isTransitionalFromCore(coreNodeName)
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
func (e *Engine) refillLoaderForPayload(loader *domain.Loader, payloadCode domain.PayloadCode) int {
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
	// No silent default. A produce manual_swap payload reaching this legacy
	// bin-count path has already been confirmed NOT opted into UOP-threshold
	// C-push (MaybeCreateLoaderEmptyIn skips threshold>0 payloads before calling
	// here). If it also carries no explicit ReorderPoint it has no replenishment
	// policy, so create nothing — replenishment is explicit opt-in (set a
	// ReorderPoint or a UOP threshold). The former magic-2 default fired a silent
	// over-supply here; ×payloads it was the SLN_002 incident's multiplier and it
	// masked loaders that were never configured.
	// Per-payload bin-count floor from the Loader: the cache's per-payload value
	// for the aggregate, the claim's ReorderPoint for legacy — projected once at
	// resolution, so no flag branch here. Zero = no bin-count policy → create nothing.
	minStock := loader.MinStockFor(payloadCode)
	if minStock <= 0 {
		e.debugFn("side-cycle: loader=%s payload=%s has no ReorderPoint and no UOP threshold — no replenishment policy, creating no L1",
			loader.Name(), payloadCode)
		return 0
	}
	currentCount := 0
	if count, ok := e.systemBinCountForPayload(string(payloadCode)); ok {
		currentCount = count
	}
	if currentCount >= minStock {
		e.logFn("side-cycle: loader=%s payload=%s currentCount=%d >= minStock=%d — skipping L1",
			loader.Name(), payloadCode, currentCount, minStock)
		return 0
	}
	// Desired total in-loop bins; tryCreateL1 subtracts what is already in
	// flight (fail-closed) and fires the remainder. The system-count
	// fail-OPEN above stays here at the caller — only the in-flight guard is
	// centralized.
	desired := minStock - currentCount
	created, err := e.tryCreateL1(loader, payloadCode, L1SideCycle, desired, "") // legacy bin-count sweep: no member named → first-match
	if err != nil {
		e.logFn("side-cycle: loader=%s payload=%s — L1 creation failed after %d created: %v",
			loader.Name(), payloadCode, created, err)
		return created
	}
	if created > 0 {
		e.logFn("side-cycle: loader=%s created=%d payload=%s minStock=%d currentCount=%d",
			loader.Name(), created, payloadCode, minStock, currentCount)
	}
	return created
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
// (config LoadersMultiWindow). Off by default — a shared loader funnels to its
// anchor until the operator board (A2) and demand re-key (B9) land.
func (e *Engine) multiWindowEnabled() bool {
	return e.cfg != nil && e.cfg.LoadersMultiWindow
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
// old manualSwapNode shim). The transitional gate is applied here; the count→fire
// atomicity, the per-payload dedup, the capacity cap, and the decision record all
// live in reserveLoaderEmpties. count is the desired total in-flight for the payload.
func (e *Engine) tryCreateL1(loader *domain.Loader, payload domain.PayloadCode, source L1Source, count int, member domain.NodeID) (int, error) {
	if loader == nil {
		return 0, nil
	}
	coreNode := string(loader.ID())
	// IsTransitional reads the aggregate directly — correct after loader.ID() became
	// the loader_key token (a cache lookup keyed on the old core_node_name would now
	// miss). The push source is exempt regardless (it IS the transitional supply path).
	if loader.IsTransitional() && source.suppressedByTransitional() {
		e.debugFn("%s: loader=%s payload=%s skipped — transitional, operator-driven",
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

// loaderInFlightEmptyCount counts non-terminal retrieve_empty orders inbound to
// the loader's CORE NODE regardless of payload tag. maybeStageLoaderEmpty uses it
// as a cheap advisory pre-check before the seam; the threshold/legacy paths now
// count per-payload AND in total inside reserveLoaderEmpties (one set query) so
// their dedup + capacity are atomic. Keyed by core node (delivery_node) so
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
	// Project the resolved manual_swap node into a Loader for the seam (the push
	// path still enumerates via findManualSwapNodes, which the unloader shares).
	dl, perr := loaderFromManualSwapClaim(loader.claim, domain.ReplenishmentAuto)
	if perr != nil {
		e.logFn("loader-push: project loader %s failed — skipping: %v", loader.node.CoreNodeName, perr)
		return
	}
	if _, err := e.tryCreateL1(dl, "", L1LoaderPush, 1, ""); err != nil { // opportunistic push: payload-agnostic, no member
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
