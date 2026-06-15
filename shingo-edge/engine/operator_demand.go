package engine

import (
	"shingoedge/store/orders"
)

// SendClaimSync is retired under the Core-owned loader aggregate. Core derives
// demand_registry from its bin_loaders aggregate (seeddev / migrateloaders), so
// the Edge's style_node_claims are no longer authoritative and pushing them up
// would clobber the Core-derived registry. Kept as a no-op so the call sites
// (startup-ack, the admin claim-edit path, the replenishment page) don't each
// need to change.
func (e *Engine) SendClaimSync() {}

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
