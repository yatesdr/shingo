package engine

import (
	"fmt"
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/store/processes"
)
// SendClaimSync builds a ClaimSync message from all manual_swap claims
// across **every style** on every process — not just the active one — and
// sends it to Core. Core uses this to populate its demand registry for
// kanban wiring and the UOP-threshold replenishment monitor.
//
// Called on startup (after registration ack) and whenever the operator
// upserts or deletes a style node claim via the admin UI (see
// shingo-edge/www/handlers_api_config.go) or applies a loader threshold
// via the replenishment page. Without the UI-triggered resync,
// demand_registry would only converge on the next heartbeat cycle or
// Edge restart.
//
// All-styles vs. active-only: the replenishment page lets engineers
// configure thresholds for (loader, payload) bindings on inactive
// styles (commissioning, calibration, multi-style processes). Those
// thresholds belong in demand_registry so Core's monitor watches them
// even when the binding's style isn't running. Active-only sync used
// to silently drop those — the symptom was "threshold applied, page
// shows it, but C-push never fires." Core-side DemandSignal handling
// for kanban is already active-style-gated on Edge
// (FindLoaderForPayload short-circuits on inactive styles), so the
// extra entries don't drive runtime work — they just make sure the
// threshold monitor has the data it needs.
//
// Claims are deduplicated by (core_node_name, role) — a loader on
// multiple styles becomes one ClaimSyncEntry with the union of
// AllowedPayloads. Active-style values win for OutboundDestination
// when they exist, so changing the running style updates the wire.
func (e *Engine) SendClaimSync() {
	stationID := e.cfg.StationID()
	processes, err := e.db.ListProcesses()
	if err != nil {
		log.Printf("claim sync: list processes: %v", err)
		return
	}

	// aggregated tracks one ClaimSyncEntry-in-progress per
	// (core_node_name, role). allowedPayloads is a set; activeKnown
	// records whether we've seen the active style for this node yet
	// so a later inactive-style claim doesn't clobber the
	// active-style OutboundDestination.
	type aggregated struct {
		coreNodeName        string
		role                protocol.ClaimRole
		outboundDestination string
		allowedPayloads     map[string]bool
		activeKnown         bool
	}
	byKey := make(map[string]*aggregated)
	keyFor := func(coreNodeName string, role protocol.ClaimRole) string {
		return coreNodeName + "\x00" + string(role)
	}

	for _, proc := range processes {
		styles, err := e.db.ListStylesByProcess(proc.ID)
		if err != nil {
			log.Printf("claim sync: list styles for process %d: %v", proc.ID, err)
			continue
		}
		activeStyleID := int64(0)
		if proc.ActiveStyleID != nil {
			activeStyleID = *proc.ActiveStyleID
		}
		for _, st := range styles {
			active := st.ID == activeStyleID
			nodeClaims, err := e.db.ListStyleNodeClaims(st.ID)
			if err != nil {
				log.Printf("claim sync: list claims for style %d: %v", st.ID, err)
				continue
			}
			for _, c := range nodeClaims {
				if c.SwapMode != protocol.SwapModeManualSwap {
					continue
				}
				payloads := c.AllowedPayloads()
				if len(payloads) == 0 {
					continue
				}
				k := keyFor(c.CoreNodeName, c.Role)
				agg, ok := byKey[k]
				if !ok {
					agg = &aggregated{
						coreNodeName:        c.CoreNodeName,
						role:                c.Role,
						outboundDestination: c.OutboundDestination,
						allowedPayloads:     make(map[string]bool),
					}
					byKey[k] = agg
				}
				// Active-style claim wins for OutboundDestination if
				// we haven't already locked it in from an earlier
				// active-style claim on the same key (multiple active
				// styles per process aren't a thing, but defending
				// against the iteration order changing is cheap).
				if active && !agg.activeKnown {
					agg.outboundDestination = c.OutboundDestination
					agg.activeKnown = true
				}
				for _, p := range payloads {
					agg.allowedPayloads[p] = true
				}
			}
		}
	}

	// Build the final ClaimSyncEntry list with each aggregated entry's
	// thresholds resolved from loader_payload_thresholds. Sorted
	// payload codes keep the wire shape deterministic for debugging.
	var claims []protocol.ClaimSyncEntry
	for _, agg := range byKey {
		payloads := make([]string, 0, len(agg.allowedPayloads))
		for p := range agg.allowedPayloads {
			payloads = append(payloads, p)
		}
		slices.Sort(payloads)
		// UOP-threshold replenishment: ship the per-payload
		// threshold map so Core can populate
		// demand_registry.replenish_uop_threshold. Threshold 0 is
		// the opt-out default; the protocol encoder omits zero
		// entries via omitempty on the map field. v6: thresholds
		// are keyed by core_node_name directly — no process_node
		// lookup needed since core_node_name is the canonical
		// cross-system identifier and the same row applies across
		// every style's claim row that lists the same loader.
		thresholds := map[string]int{}
		if agg.role == protocol.ClaimRoleProduce {
			rows, err := e.db.ThresholdsByPayloadForLoader(agg.coreNodeName)
			if err != nil {
				// Do NOT silently swallow this: an empty threshold map ships
				// to Core, which (via omitempty) reads it as "no thresholds"
				// and demotes this loader from C-push to legacy bin-count
				// until the next successful sync. It was previously dropped on
				// the floor with no log. Surface it so the strategy flip is
				// diagnosable. (A safer fix — skip emitting the resync so Core
				// keeps its last-known registry — depends on Core's
				// SyncRegistry merge semantics; deferred to SME review.)
				log.Printf("claim sync: thresholds for loader %s failed, shipping without (loader falls back to legacy bin-count at Core): %v",
					agg.coreNodeName, err)
			} else {
				for _, p := range payloads {
					if v, ok := rows[p]; ok && v > 0 {
						thresholds[p] = v
					}
				}
			}
		}
		claims = append(claims, protocol.ClaimSyncEntry{
			CoreNodeName:        agg.coreNodeName,
			Role:                agg.role,
			AllowedPayloadCodes: payloads,
			OutboundDestination: agg.outboundDestination,
			PayloadThresholds:   thresholds,
		})
	}

	sync := &protocol.ClaimSync{
		StationID: stationID,
		Claims:    claims,
	}

	env, err := protocol.NewDataEnvelope(
		protocol.SubjectClaimSync,
		protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		protocol.Address{Role: protocol.RoleCore},
		sync,
	)
	if err != nil {
		log.Printf("claim sync: build envelope: %v", err)
		return
	}
	if err := e.SendEnvelope(env); err != nil {
		log.Printf("claim sync: send: %v", err)
		return
	}
	log.Printf("claim sync: sent %d claims to core", len(claims))
}

// manualSwapNode pairs a manual_swap claim with its matching process node.
type manualSwapNode struct {
	node  processes.Node
	claim processes.NodeClaim
}

// findManualSwapNodes returns all (node, claim) pairs where the claim has
// SwapMode=="manual_swap" across all active processes. If coreNodeName is
// non-empty, only nodes matching that name are returned.
func (e *Engine) findManualSwapNodes(coreNodeName string) []manualSwapNode {
	var results []manualSwapNode
	err := processes.WalkClaims(e.db.DB, processes.WalkOpts{
		ActiveOnly:   true,
		SwapMode:     protocol.SwapModeManualSwap,
		CoreNodeName: coreNodeName,
		ResolveNode:  true,
	}, func(ctx processes.WalkCtx) bool {
		results = append(results, manualSwapNode{node: ctx.Node, claim: ctx.Claim})
		return false // collect all matches
	})
	if err != nil {
		log.Printf("findManualSwapNodes: %v", err)
		return nil
	}
	return results
}

// FindLoaderForPayload returns the (node, claim) pair for the manual_swap
// PRODUCER claim that matches the given payload code, or nil if none exists.
// Producer = bin loader: a station where an operator manually fills empty
// bins. Consumer manual_swap nodes (unloaders) are NOT returned here; use
// FindUnloaderForPayload for that side.
//
// Used by the side-cycle order generator: when a line REQUEST creates demand
// for a payload, the engine creates a parallel "empty-in" order tracked at
// the loader so the loader operator's UI surfaces the demand directly.
func (e *Engine) FindLoaderForPayload(payloadCode string) *manualSwapNode {
	if payloadCode == "" {
		return nil
	}
	var found *manualSwapNode
	err := processes.WalkClaims(e.db.DB, processes.WalkOpts{
		ActiveOnly:  true,
		Role:        protocol.ClaimRoleProduce,
		SwapMode:    protocol.SwapModeManualSwap,
		PayloadCode: payloadCode,
		ResolveNode: true,
	}, func(ctx processes.WalkCtx) bool {
		m := manualSwapNode{node: ctx.Node, claim: ctx.Claim}
		found = &m
		return true // first match
	})
	if err != nil {
		log.Printf("FindLoaderForPayload: %v", err)
		return nil
	}
	return found
}

// FindAnyLoaderClaimForPayload returns a (node, claim) pair for a
// manual_swap PRODUCER claim matching the payload across **every**
// style on every process, not just the active style. Returns the
// first match. Used by:
//
//  1. The engineer-triggered Calculate path to resolve bin capacity —
//     a payload may be on an inactive style during commissioning,
//     calibration, or multi-process plants, and the calculator still
//     needs to know the bin's UOPCapacity so the UI can render the
//     implied-bin annotation ("≈ N bins") next to the calculated
//     threshold.
//
//  2. The UOP-threshold L1 trigger path (HandleLoopBelowThreshold,
//     Round-3 Obs 9). Threshold-driven demand is pre-stock semantics
//     — Core decides "this loop needs replenishment for THIS payload"
//     based on the configured threshold, and the threshold belongs to
//     the loader claim, not the style. An inactive-style loader still
//     pulls empties on threshold because the operator pre-stocks for
//     the upcoming changeover. The Item C planStore capacity gate is
//     the safety net here: if storage downstream is full, the L1
//     queues at Core rather than spamming dispatch.
//
// Line-driven demand (HandleDemandSignal — operator counter pulls)
// must NOT use this helper; it stays active-style-gated to keep
// counters bound to the running style. SendClaimSync is separately
// scoped — see commit 39df43b (2026-05-18) which extended the sync to
// include all-style claims so Core's threshold registry has the data
// it needs.
func (e *Engine) FindAnyLoaderClaimForPayload(payloadCode string) *manualSwapNode {
	if payloadCode == "" {
		return nil
	}
	var found *manualSwapNode
	// ActiveOnly omitted (false): walk every style, not just the active one —
	// a threshold-driven L1 can target a loader bound to an inactive style.
	err := processes.WalkClaims(e.db.DB, processes.WalkOpts{
		Role:        protocol.ClaimRoleProduce,
		SwapMode:    protocol.SwapModeManualSwap,
		PayloadCode: payloadCode,
		ResolveNode: true,
	}, func(ctx processes.WalkCtx) bool {
		m := manualSwapNode{node: ctx.Node, claim: ctx.Claim}
		found = &m
		return true // first match
	})
	if err != nil {
		log.Printf("FindAnyLoaderClaimForPayload: %v", err)
		return nil
	}
	return found
}

// FindLoaderClaimAt resolves the produce manual_swap loader claim for the
// given (coreNodeName, payloadCode) across every style — the node-targeted
// counterpart to FindAnyLoaderClaimForPayload. Threshold (C-push) signals
// carry the authoritative loader CoreNodeName, so they resolve by it: when
// the same payload is loaded at two loaders, resolving by payload alone would
// fire the L1 at whichever loader the walk hits first, not the one Core
// signaled. All-styles (Round-3 Obs 9: an inactive-style loader still
// receives threshold-driven L1s). Returns nil if no matching claim exists.
func (e *Engine) FindLoaderClaimAt(coreNodeName, payloadCode string) *manualSwapNode {
	if coreNodeName == "" || payloadCode == "" {
		return nil
	}
	var found *manualSwapNode
	err := processes.WalkClaims(e.db.DB, processes.WalkOpts{
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
		log.Printf("FindLoaderClaimAt: %v", err)
		return nil
	}
	return found
}

// FindUnloaderForPayload returns the (node, claim) pair for the manual_swap
// CONSUMER claim matching the payload, or nil. Symmetric to
// FindLoaderForPayload — the side-cycle model handles unloaders the same
// way: when a line evac sends a full bin out, the engine creates a parallel
// "full-in" order tracked at the unloader so the operator's UI sees it.
func (e *Engine) FindUnloaderForPayload(payloadCode string) *manualSwapNode {
	if payloadCode == "" {
		return nil
	}
	var found *manualSwapNode
	err := processes.WalkClaims(e.db.DB, processes.WalkOpts{
		ActiveOnly:  true,
		Role:        protocol.ClaimRoleConsume,
		SwapMode:    protocol.SwapModeManualSwap,
		PayloadCode: payloadCode,
		ResolveNode: true,
	}, func(ctx processes.WalkCtx) bool {
		m := manualSwapNode{node: ctx.Node, claim: ctx.Claim}
		found = &m
		return true // first match
	})
	if err != nil {
		log.Printf("FindUnloaderForPayload: %v", err)
		return nil
	}
	return found
}

// countLoaderInFlightEmptyIn returns the number of non-terminal
// retrieve_empty orders the loader at nodeID has for the payload.
// MaybeCreateLoaderEmptyIn uses this against ReorderPoint to top up the
// in-flight queue to (ReorderPoint - currentCount) instead of capping at
// one — operators get the full demand visible at once rather than one
// queue per demand signal.
//
// Returns an error (not an in-band sentinel) on a DB read failure so
// tryCreateL1 can fail closed — fire nothing rather than into the dark when
// the order list is unavailable.
func (e *Engine) countLoaderInFlightEmptyIn(nodeID int64, payloadCode string) (int, error) {
	orderList, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		return 0, fmt.Errorf("list active orders for node %d: %w", nodeID, err)
	}
	n := 0
	for _, o := range orderList {
		if o.PayloadCode == payloadCode && o.RetrieveEmpty {
			n++
		}
	}
	return n, nil
}

// unloaderHasInFlightFullIn reports whether the unloader at nodeID already
// has a non-terminal retrieve order (full-bin retrieve) for the payload.
// Symmetric to loaderHasInFlightEmptyIn — dedupes a flurry of line evac
// events from queuing a stack of full-in mirror orders at the unloader.
func (e *Engine) unloaderHasInFlightFullIn(nodeID int64, payloadCode string) bool {
	orderList, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		e.logFn("side-cycle: list active orders for node %d: %v", nodeID, err)
		return true
	}
	for _, o := range orderList {
		// Full-bin retrieve = retrieve order with payload code, NOT marked
		// as retrieve_empty. The unloader's mirror order shape.
		if o.PayloadCode == payloadCode && !o.RetrieveEmpty {
			return true
		}
	}
	return false
}

// unloaderHasUsableFullPresent is the consumer-side counterpart to the
// removed loaderHasUsableEmptyPresent: skips the U1 full-in retrieve when
// Core reports a full bin of the target payload already physically at the
// unloader. Fails OPEN — if Core is unreachable or returns no data, falls
// through to the in-flight order check and assumes the floor is empty.
func (e *Engine) unloaderHasUsableFullPresent(coreNodeName, payloadCode string) bool {
	if !e.coreClient.Available() || coreNodeName == "" || payloadCode == "" {
		return false
	}
	bins, _ := e.coreClient.FetchNodeBins([]string{coreNodeName})
	if len(bins) == 0 {
		return false
	}
	b := bins[0]
	return b.Occupied && b.PayloadCode == payloadCode
}

// systemBinCountForPayload reports how many bins of payloadCode are in
// the kanban loop system-wide via Core's /api/inventory/system-count
// endpoint (see shingo-core/service/inventory_system_count.go). This
// counts bins anywhere in the active lifecycle (available, staged) —
// at storage, in transit, staged at consumer lines, being filled at
// loaders. Excludes bins production can't rely on: flagged,
// maintenance, quality_hold, retired.
//
// This is INTENTIONALLY NOT PreflightInventory. Pre-2026-05-11 this
// helper called PreflightInventory, which has "available for sourcing
// right now" semantics: it excludes staged bins, claimed bins, and
// non-storage nodes. That mismatch caused the SNF2 plant incident
// (76682-6TA0A.06 at ReorderPoint=2, system held 2 bins total but
// PreflightInventory only saw 1 of them — the one at storage — so L1
// kept firing). System-count answers the question the kanban math
// actually wants: how many physical bins are still in the loop.
//
// The second return is false when the count couldn't be obtained — Core
// unreachable, empty payload, or HTTP error. Callers fail OPEN at the
// use site (treat as zero) for the same reason loaderHasUsableEmptyPresent
// does: a missed L1 leaves the loader idle; a redundant L1 is dedup'd by
// the in-flight guard. Idle is the worse outcome.
func (e *Engine) systemBinCountForPayload(payloadCode string) (int, bool) {
	if !e.coreClient.Available() || payloadCode == "" {
		return 0, false
	}
	counts, ok := e.coreClient.SystemBinCount([]string{payloadCode})
	if !ok {
		e.logFn("side-cycle: system-count for %s: core unreachable or error", payloadCode)
		return 0, false
	}
	for _, c := range counts {
		if c.PayloadCode == payloadCode {
			return c.BinCount, true
		}
	}
	return 0, true // payload absent from result = 0 bins
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
	inFlight, err := e.countLoaderInFlightEmptyIn(loader.node.ID, payload)
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

// MaybeCreateUnloaderFullIn (U1 of the side-cycle model) is the consumer-side
// counterpart to MaybeCreateLoaderEmptyIn. When the line releases a full bin
// of payloadCode (DispositionCaptureLineside on a produce-role claim), this
// creates a parallel "full-in" retrieve order tracked at the unloader so the
// unloader operator's UI surfaces the demand directly. Without this mirror,
// the unloader sees nothing — the line's evac order is tracked at the LINE's
// process_node, not the unloader's.
//
// U2 (empty-out from the unloader to the supermarket) fires when the unloader
// operator confirms that the bin's contents have been processed — symmetric
// to L2. See handleUnloaderFullInCompletion in wiring_completion.go.
//
// Caller: ReleaseOrderWithLineside in operator_release.go fires this for
// produce-role releases under DispositionCaptureLineside, mirroring the L1
// trigger for consume-role.
func (e *Engine) MaybeCreateUnloaderFullIn(payloadCode string) {
	unloader := e.FindUnloaderForPayload(payloadCode)
	if unloader == nil {
		return
	}
	e.createUnloaderFullIn(*unloader, payloadCode)
}

// createUnloaderFullIn fires a U1 retrieve_full at an ALREADY-RESOLVED
// unloader if none is in flight and no full bin is parked at the window.
// Split out from MaybeCreateUnloaderFullIn so the push/sweep paths — which
// already hold the resolved node from their own walk — don't re-resolve it
// per payload via FindUnloaderForPayload (a full claim-tree walk).
func (e *Engine) createUnloaderFullIn(unloader manualSwapNode, payloadCode string) {
	if e.unloaderHasInFlightFullIn(unloader.node.ID, payloadCode) {
		e.logFn("side-cycle: unloader %s already has in-flight full-in for %s, skipping",
			unloader.node.Name, payloadCode)
		return
	}
	if e.unloaderHasUsableFullPresent(unloader.node.CoreNodeName, payloadCode) {
		e.logFn("side-cycle: unloader %s already has a full bin (%s) parked, skipping U1",
			unloader.node.Name, payloadCode)
		return
	}
	nodeID := unloader.node.ID
	// U1 (Unloader Full In) must NEVER auto-confirm. Same reasoning as L1:
	// the unloader operator is an active participant — they need to
	// physically process the bin's contents and confirm explicitly. Auto-
	// confirming here would immediately fire U2 (empty-out to supermarket)
	// before any processing has happened, with the bin still full. Honoring
	// global cfg.Web.AutoConfirm at this layer defeats the side-cycle model.
	autoConfirm := false
	// Source group: unloader.claim.InboundSource — the FG supermarket the
	// unloader pulls full bins from. Empty falls back to Core's global FIFO
	// (the historical behaviour, preserved when InboundSource isn't set).
	// This mirror order's primary purpose is UI demand surfacing, not
	// driving robot movement (the line's evac drives that), but the source
	// still needs to be group-aware so multi-supermarket plants don't
	// surface demand against the wrong store.
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, false, 1, unloader.node.CoreNodeName, unloader.claim.InboundSource, "",
		"standard", payloadCode, autoConfirm, true,
	)
	if err != nil {
		e.logFn("side-cycle: create full-in order for unloader %s payload %s: %v",
			unloader.node.Name, payloadCode, err)
		return
	}
	log.Printf("side-cycle: full-in order %d for unloader %s payload %s",
		order.ID, unloader.node.Name, payloadCode)
}

// MaybePushUnloader is the auto-push trigger for consume manual_swap (unloader)
// claims with AutoPush=true. It walks the unloader's allowed payloads and
// fires a U1 retrieve_full for any payload not already in-flight or parked
// at the window. Unlike MaybeCreateUnloaderFullIn (which is called from line
// evac and targets ONE specific payload that just left the line), this push
// is window-driven: it asks "given this unloader is free, is there ANY allowed
// payload available upstream to pull in?" and creates orders accordingly.
//
// Trigger sites:
//   - ClearBin completion (operator confirmed unload — window just freed).
//   - handleManualSwapCompletion U2-arrived (empty returned to supermarket
//     — window confirmed clear).
//   - SweepPushUnloaders on Edge startup / registration ack — catches windows
//     that became free while Edge was offline.
//
// No-op if claim isn't AutoPush, isn't manual_swap consume, or all allowed
// payloads are already covered. Dedupe relies on the same in-flight /
// usable-present checks MaybeCreateUnloaderFullIn uses; we delegate to it.
//
// nodeID names a specific unloader (typically the one whose window just
// freed). Pass 0 for an "any unloader" sweep — see SweepPushUnloaders.
func (e *Engine) MaybePushUnloader(nodeID int64) {
	matches := e.findManualSwapNodes("")
	for _, m := range matches {
		if nodeID != 0 && m.node.ID != nodeID {
			continue
		}
		if m.claim.Role != protocol.ClaimRoleConsume {
			continue
		}
		if !m.claim.AutoPush {
			continue
		}
		// Each allowed payload gets its own MaybeCreateUnloaderFullIn pass.
		// That helper already short-circuits on in-flight + window-occupied.
		// One payload per allowed code at most — the unloader window holds
		// a single bin, but the multi-order queue lets us stage the next
		// few in Core and dispatch them as the window frees.
		for _, code := range m.claim.AllowedPayloads() {
			e.createUnloaderFullIn(m, code)
		}
	}
}

// SweepPushUnloaders walks every active consume manual_swap claim with
// AutoPush=true and fires MaybePushUnloader. Intended for Edge startup
// (after registration ack, mirroring SendClaimSync). Catches the case
// where the unloader was empty when Edge went down and supply became
// available while it was offline — without this, the window stays empty
// until the next ClearBin/U2 completion.
func (e *Engine) SweepPushUnloaders() {
	if !e.sweepingUnloaders.CompareAndSwap(false, true) {
		return // a sweep is already running — a re-register storm must not stack them
	}
	defer e.sweepingUnloaders.Store(false)
	matches := e.findManualSwapNodes("")
	swept := 0
	for _, m := range matches {
		if m.claim.Role != protocol.ClaimRoleConsume || !m.claim.AutoPush {
			continue
		}
		for _, code := range m.claim.AllowedPayloads() {
			e.createUnloaderFullIn(m, code)
		}
		swept++
	}
	if swept > 0 {
		log.Printf("auto-push: startup sweep covered %d unloader claim(s)", swept)
	}
}

// loaderInFlightEmptyCount counts non-terminal retrieve_empty orders at the
// loader regardless of payload tag. MaybePushLoader uses it to keep exactly
// one empty staged; countLoaderInFlightEmptyIn is the per-payload variant the
// threshold/legacy paths use.
func (e *Engine) loaderInFlightEmptyCount(nodeID int64) (int, error) {
	orderList, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		return 0, fmt.Errorf("list active orders for node %d: %w", nodeID, err)
	}
	n := 0
	for _, o := range orderList {
		if o.RetrieveEmpty {
			n++
		}
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
// already in flight. The empty is a generic carrier tagged with the loader's
// representative payload; the operator re-binds the actual payload at load
// time (the multi-payload LoadBin re-bind). One-at-a-time keeps it
// opportunistic; L1LoaderPush is exempt from the transitional suppression in
// tryCreateL1 (it IS the transitional supply path).
func (e *Engine) maybeStageLoaderEmpty(loader manualSwapNode) {
	payloads := loader.claim.AllowedPayloads()
	if len(payloads) == 0 {
		return // misconfigured loader — nothing to stage against
	}
	inFlight, err := e.loaderInFlightEmptyCount(loader.node.ID)
	if err != nil {
		e.logFn("loader-push: in-flight lookup at %s failed — skipping: %v", loader.node.CoreNodeName, err)
		return
	}
	if inFlight > 0 {
		e.debugFn("loader-push: %s already has %d empty in flight — skipping", loader.node.CoreNodeName, inFlight)
		return
	}
	if _, err := e.tryCreateL1(&loader, payloads[0], L1LoaderPush, 1); err != nil {
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
