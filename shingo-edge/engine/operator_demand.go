package engine

import (
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// SendClaimSync is retired under the Core-owned loader aggregate. Core derives
// demand_registry from its bin_loaders aggregate (seeddev / migrateloaders), so
// the Edge's style_node_claims are no longer authoritative and pushing them up
// would clobber the Core-derived registry. Kept as a no-op so the call sites
// (startup-ack, the admin claim-edit path, the replenishment page) don't each
// need to change.
func (e *Engine) SendClaimSync() {}

// manualSwapNode pairs a manual_swap claim with its matching process node. It is
// projected from the Core-owned loader aggregate (manualSwapNodesFromCore) — the
// loader push/board enumeration still consumes this shape.
type manualSwapNode struct {
	node  processes.Node
	claim processes.NodeClaim
}

// findManualSwapNodes returns all (node, claim) pairs for manual_swap loaders,
// projected from the Core loader aggregate. If coreNodeName is non-empty, only
// nodes matching that name are returned.
func (e *Engine) findManualSwapNodes(coreNodeName string) []manualSwapNode {
	return e.manualSwapNodesFromCore(coreNodeName)
}

// FindLoaderForPayload returns the manual_swap PRODUCER (bin loader) for the
// payload, or nil. Resolved from the Core aggregate.
func (e *Engine) FindLoaderForPayload(payloadCode string) *manualSwapNode {
	if payloadCode == "" {
		return nil
	}
	return e.resolveCoreLoaderForPayload("", payloadCode, "produce")
}

// FindAnyLoaderClaimForPayload resolves a produce loader for the payload across
// every style (the engineer Calculate path + the UOP-threshold L1 trigger, which
// can target an inactive-style loader). Resolved from the Core aggregate.
func (e *Engine) FindAnyLoaderClaimForPayload(payloadCode string) *manualSwapNode {
	if payloadCode == "" {
		return nil
	}
	return e.resolveCoreLoaderForPayload("", payloadCode, "produce")
}

// FindLoaderClaimAt resolves the produce loader for (coreNodeName, payloadCode) —
// the node-targeted form a threshold (C-push) signal carries so the same payload
// loaded at two loaders fires the L1 at the one Core signaled. Resolved from the
// Core aggregate.
func (e *Engine) FindLoaderClaimAt(coreNodeName, payloadCode string) *manualSwapNode {
	if coreNodeName == "" || payloadCode == "" {
		return nil
	}
	return e.resolveCoreLoaderForPayload(coreNodeName, payloadCode, "produce")
}

// FindUnloaderForPayload returns the manual_swap CONSUMER (unloader) for the
// payload, or nil. Resolved from the Core aggregate.
func (e *Engine) FindUnloaderForPayload(payloadCode string) *manualSwapNode {
	if payloadCode == "" {
		return nil
	}
	return e.resolveCoreLoaderForPayload("", payloadCode, "consume")
}

// systemBinCountForPayload reports how many bins of payloadCode are in the kanban
// loop system-wide via Core's /api/inventory/system-count endpoint. This counts
// bins anywhere in the active lifecycle (available, staged) — at storage, in
// transit, staged at consumer lines, being filled at loaders. Excludes bins
// production can't rely on: flagged, maintenance, quality_hold, retired.
//
// INTENTIONALLY NOT PreflightInventory. Pre-2026-05-11 this called
// PreflightInventory, which has "available for sourcing right now" semantics
// (excludes staged/claimed bins and non-storage nodes). That mismatch caused the
// SNF2 incident (76682-6TA0A.06 at ReorderPoint=2: system held 2 bins but
// PreflightInventory saw 1, so L1 kept firing). System-count answers the question
// the kanban math wants: how many physical bins are still in the loop.
//
// Second return is false when the count couldn't be obtained (Core unreachable,
// empty payload, HTTP error). Callers fail OPEN at the use site (treat as zero):
// a missed L1 leaves the loader idle; a redundant L1 is dedup'd by the in-flight
// guard. Idle is the worse outcome.
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

// countActiveOrdersAtNode lists the non-terminal orders delivering to a core node
// and counts those matching pred — the shared list+scan body behind the per-role
// in-flight tallies. Keyed by core node (delivery_node) so a shared loader's
// sibling process_node rows don't under-count; see
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
