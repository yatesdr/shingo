package engine

import (
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/store/processes"
)
// SendClaimSync builds a ClaimSync message from all manual_swap claims across
// all active processes and sends it to Core. Core uses this to populate its
// demand registry for kanban wiring.
//
// Called on startup (after registration ack) and whenever the operator
// upserts or deletes a style node claim via the admin UI (see
// shingo-edge/www/handlers_api_config.go). Without the UI-triggered
// resync, demand_registry would only converge on the next heartbeat
// cycle or Edge restart.
func (e *Engine) SendClaimSync() {
	stationID := e.cfg.StationID()
	processes, err := e.db.ListProcesses()
	if err != nil {
		log.Printf("claim sync: list processes: %v", err)
		return
	}

	var claims []protocol.ClaimSyncEntry
	for _, proc := range processes {
		if proc.ActiveStyleID == nil {
			continue
		}
		nodeClaims, err := e.db.ListStyleNodeClaims(*proc.ActiveStyleID)
		if err != nil {
			log.Printf("claim sync: list claims for style %d: %v", *proc.ActiveStyleID, err)
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
			claims = append(claims, protocol.ClaimSyncEntry{
				CoreNodeName:        c.CoreNodeName,
				Role:                c.Role,
				AllowedPayloadCodes: payloads,
				OutboundDestination: c.OutboundDestination,
			})
		}
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
	processList, err := e.db.ListProcesses()
	if err != nil {
		log.Printf("findManualSwapNodes: list processes: %v", err)
		return nil
	}

	var results []manualSwapNode
	for _, proc := range processList {
		if proc.ActiveStyleID == nil {
			continue
		}
		claims, err := e.db.ListStyleNodeClaims(*proc.ActiveStyleID)
		if err != nil {
			log.Printf("findManualSwapNodes: list claims for style %d: %v", *proc.ActiveStyleID, err)
			continue
		}

		// Fetch nodes once per process, not once per claim (fixes pre-existing N+1).
		var nodes []processes.Node
		var nodesFetched bool
		for _, claim := range claims {
			if claim.SwapMode != protocol.SwapModeManualSwap {
				continue
			}
			if coreNodeName != "" && claim.CoreNodeName != coreNodeName {
				continue
			}
			if !nodesFetched {
				nodes, err = e.db.ListProcessNodesByProcess(proc.ID)
				if err != nil {
					log.Printf("findManualSwapNodes: list nodes for process %d: %v", proc.ID, err)
					break
				}
				nodesFetched = true
			}
			for _, node := range nodes {
				if node.CoreNodeName != claim.CoreNodeName {
					continue
				}
				results = append(results, manualSwapNode{node: node, claim: claim})
			}
		}
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
// See SHINGO_TODO.md "Bin loader as active workflow participant".
func (e *Engine) FindLoaderForPayload(payloadCode string) *manualSwapNode {
	if payloadCode == "" {
		return nil
	}
	for _, m := range e.findManualSwapNodes("") {
		if m.claim.Role != protocol.ClaimRoleProduce {
			continue
		}
		if !slices.Contains(m.claim.AllowedPayloads(), payloadCode) {
			continue
		}
		return &m
	}
	return nil
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
	for _, m := range e.findManualSwapNodes("") {
		if m.claim.Role != protocol.ClaimRoleConsume {
			continue
		}
		if !slices.Contains(m.claim.AllowedPayloads(), payloadCode) {
			continue
		}
		return &m
	}
	return nil
}

// countLoaderInFlightEmptyIn returns the number of non-terminal
// retrieve_empty orders the loader at nodeID has for the payload.
// MaybeCreateLoaderEmptyIn uses this against ReorderPoint to top up the
// in-flight queue to (ReorderPoint - currentCount) instead of capping at
// one — operators get the full demand visible at once rather than one
// queue per demand signal.
//
// Returns -1 on a DB error so callers can fail closed (treat as "already
// at cap, don't fire more") and avoid piling up if Core is unreachable.
func (e *Engine) countLoaderInFlightEmptyIn(nodeID int64, payloadCode string) int {
	orderList, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		e.logFn("side-cycle: list active orders for node %d: %v", nodeID, err)
		return -1
	}
	n := 0
	for _, o := range orderList {
		if o.PayloadCode == payloadCode && o.RetrieveEmpty {
			n++
		}
	}
	return n
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
func (e *Engine) MaybeCreateLoaderEmptyIn(payloadCode string) {
	loader := e.FindLoaderForPayload(payloadCode)
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
	for _, code := range loader.claim.AllowedPayloads() {
		e.refillLoaderForPayload(loader, code)
	}
}

// refillLoaderForPayload tops the per-payload empty-in queue at one loader
// up to (ReorderPoint - currentCount - inFlight) orders. Per-payload helper
// so MaybeCreateLoaderEmptyIn can sweep across all allowed payloads.
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
	inFlight := e.countLoaderInFlightEmptyIn(loader.node.ID, payloadCode)
	if inFlight < 0 {
		// DB error — fail closed; the next signal will retry.
		e.logFn("side-cycle: loader %s — in-flight count lookup failed for %s, skipping",
			loader.node.Name, payloadCode)
		return
	}
	needed := minStock - currentCount - inFlight
	if needed <= 0 {
		e.logFn("side-cycle: loader %s — %d in-flight + %d in system >= %d minimum, skipping L1 for %s",
			loader.node.Name, inFlight, currentCount, minStock, payloadCode)
		return
	}
	e.logFn("side-cycle: loader %s — creating %d L1 retrieve_empty for %s (minStock=%d currentCount=%d inFlight=%d)",
		loader.node.Name, needed, payloadCode, minStock, currentCount, inFlight)

	nodeID := loader.node.ID
	// L1 (Loader Empty In) must NEVER auto-confirm. The loader operator is an
	// active participant in the side-cycle and must explicitly confirm that
	// the bin has been filled with parts. Auto-confirming here would
	// immediately trigger L2 (handleLoaderEmptyInCompletion → CreateMoveOrder)
	// and send the still-empty bin back to the supermarket. Honoring
	// loader.claim.AutoConfirm or cfg.Web.AutoConfirm at this layer defeats
	// the side-cycle model. Plant test 2026-04-27 reproduced this on plants
	// with global auto-confirm enabled.
	autoConfirm := false
	for i := 0; i < needed; i++ {
		order, err := e.orderMgr.CreateRetrieveOrder(
			&nodeID, true, 1, loader.node.CoreNodeName, "",
			"standard", payloadCode, autoConfirm, true,
		)
		if err != nil {
			e.logFn("side-cycle: create empty-in order %d/%d for loader %s payload %s: %v",
				i+1, needed, loader.node.Name, payloadCode, err)
			return
		}
		log.Printf("side-cycle: empty-in order %d (%d/%d) for loader %s payload %s",
			order.ID, i+1, needed, loader.node.Name, payloadCode)
	}
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
	// Source is left to Core's planner (FindSourceFIFO) which finds an
	// unclaimed full bin matching the payload. The unloader's CoreNodeName
	// is the destination; the line's evac order will move the actual bin.
	// This mirror order's primary purpose is UI demand surfacing, not
	// driving robot movement (the line's evac drives that).
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, false, 1, unloader.node.CoreNodeName, "",
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
