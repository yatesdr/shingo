package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/store/orders"
)

// unloaderHasInFlightFullIn reports whether the unloader's CORE NODE already
// has a non-terminal retrieve order (full-bin retrieve) for the payload.
// Symmetric to the loader empty-in count — dedupes a flurry of line evac events
// from queuing a stack of full-in mirror orders at the unloader. Keyed by core
// node (delivery_node) so a shared unloader's process_node rows don't each
// under-count; see [[shingo_manual_swap_core_node_scoping]].
func (e *Engine) unloaderHasInFlightFullIn(coreNodeName string, payloadCode string) bool {
	// Full-bin retrieve = retrieve order with payload code, NOT marked as
	// retrieve_empty. The unloader's mirror order shape.
	n, err := e.countActiveOrdersAtNode(coreNodeName, func(o orders.Order) bool {
		return o.PayloadCode == payloadCode && !o.RetrieveEmpty
	})
	if err != nil {
		e.logFn("side-cycle: list active orders for node %s: %v", coreNodeName, err)
		return true
	}
	return n > 0
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
	if e.unloaderHasInFlightFullIn(unloader.node.CoreNodeName, payloadCode) {
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
