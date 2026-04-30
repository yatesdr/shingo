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
			if c.SwapMode != "manual_swap" {
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
			if claim.SwapMode != "manual_swap" {
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
		if m.claim.Role != "produce" {
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
		if m.claim.Role != "consume" {
			continue
		}
		if !slices.Contains(m.claim.AllowedPayloads(), payloadCode) {
			continue
		}
		return &m
	}
	return nil
}

// loaderHasInFlightEmptyIn reports whether the loader at nodeID already has a
// non-terminal retrieve_empty order for the payload. Used to dedupe so a
// flurry of line requests doesn't queue a stack of empty-in orders behind a
// loader that's still working through the previous one.
func (e *Engine) loaderHasInFlightEmptyIn(nodeID int64, payloadCode string) bool {
	orderList, err := e.db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		// Fail closed: assume there's an in-flight order so we don't pile up.
		e.logFn("side-cycle: list active orders for node %d: %v", nodeID, err)
		return true
	}
	for _, o := range orderList {
		if o.PayloadCode == payloadCode && o.RetrieveEmpty {
			return true
		}
	}
	return false
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

// loaderHasUsableEmptyPresent reports whether Core telemetry shows an empty
// bin already physically at the loader. The side-cycle's L1 retrieve_empty
// is meant to bring an empty TO the loader so the operator can fill it; if
// one is already there (e.g., a previous retrieve was cancelled but the
// bin remained), firing another L1 wedges the floor — Core dispatches a
// retrieve to a station that already has its bin, then later evicts the
// parked one. Plant 2026-04-28 incident #483→#484 was this pattern.
//
// Fails OPEN: if Core is unreachable or returns no data, we fall through to
// the in-flight order check and assume the floor is empty. False-negative
// here = one redundant retrieve (caught by the in-flight dedup on the next
// REQUEST), false-positive = loader sits idle waiting for a bin that's
// already there. Idle is the worse outcome.
func (e *Engine) loaderHasUsableEmptyPresent(coreNodeName string) bool {
	if !e.coreClient.Available() || coreNodeName == "" {
		return false
	}
	bins, _ := e.coreClient.FetchNodeBins([]string{coreNodeName})
	if len(bins) == 0 {
		return false
	}
	b := bins[0]
	return b.Occupied && b.PayloadCode == ""
}

// unloaderHasUsableFullPresent is the consumer-side counterpart: skips the
// U1 full-in retrieve when Core reports a full bin of the target payload
// already physically at the unloader. Same fail-open contract as
// loaderHasUsableEmptyPresent.
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
	if e.loaderHasInFlightEmptyIn(loader.node.ID, payloadCode) {
		e.logFn("side-cycle: loader %s already has in-flight empty-in for %s, skipping",
			loader.node.Name, payloadCode)
		return
	}
	if e.loaderHasUsableEmptyPresent(loader.node.CoreNodeName) {
		e.logFn("side-cycle: loader %s already has an empty bin parked, skipping L1 for %s",
			loader.node.Name, payloadCode)
		return
	}
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
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, true, 1, loader.node.CoreNodeName, "",
		"standard", payloadCode, autoConfirm,
	)
	if err != nil {
		e.logFn("side-cycle: create empty-in order for loader %s payload %s: %v",
			loader.node.Name, payloadCode, err)
		return
	}
	log.Printf("side-cycle: empty-in order %d for loader %s payload %s",
		order.ID, loader.node.Name, payloadCode)
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
		"standard", payloadCode, autoConfirm,
	)
	if err != nil {
		e.logFn("side-cycle: create full-in order for unloader %s payload %s: %v",
			unloader.node.Name, payloadCode, err)
		return
	}
	log.Printf("side-cycle: full-in order %d for unloader %s payload %s",
		order.ID, unloader.node.Name, payloadCode)
}
