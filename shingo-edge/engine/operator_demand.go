package engine

import (
	"shingoedge/store/orders"
)

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
