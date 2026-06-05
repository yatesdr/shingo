package engine

import (
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/store/orders"
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

// countActiveOrdersAtNode lists the non-terminal orders delivering to a core
// node and counts those matching pred — the shared list+scan body behind the
// per-role in-flight tallies (loader empty-in, unloader full-in, any-empty
// staging, and the bin-ops anti-spam guard; previously four copies). Each
// caller supplies its predicate, wraps the returned error, and decides how to
// treat a read failure. Keyed by core node (delivery_node) so a shared loader's
// sibling process_node rows don't under-count — see
// [[shingo_manual_swap_core_node_scoping]].
func (e *Engine) countActiveOrdersAtNode(coreNodeName string, pred func(orders.Order) bool) (int, error) {
	orderList, err := e.db.ListActiveOrdersByDeliveryNode(coreNodeName)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range orderList {
		if pred(o) {
			n++
		}
	}
	return n, nil
}
