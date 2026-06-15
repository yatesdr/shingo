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

// (systemBinCountForPayload was retired with the bin-count floor: the produce
// supply paths are now UOP-threshold / operator-push, neither of which needs a
// system-wide physical bin count. Core's /api/inventory/system-count endpoint and
// the coreClient.SystemBinCount method remain for other callers.)

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
