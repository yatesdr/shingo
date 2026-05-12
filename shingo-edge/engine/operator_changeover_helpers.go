// operator_changeover_helpers.go — small predicates and guards shared
// across the changeover phase files (and operator_node_changeover.go).

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// findNodeByCoreName finds a process node by its CoreNodeName.
func findNodeByCoreName(nodes []processes.Node, coreName string) *processes.Node {
	for i := range nodes {
		if nodes[i].CoreNodeName == coreName {
			return &nodes[i]
		}
	}
	return nil
}

// DEAD CODE — no callers in the package. Preserved during the
// 2026-05-11 changeover-ops split for archaeology only; safe to delete.
// Originally intended to tell the operator "this leg is still expected
// to reach staged on its own; come back in a moment." If you find a
// use for it, move it out of this dead-code block and document the
// caller.
//
// isPendingOrderStatus reports whether the order is alive but not yet at
// staged — i.e., it's expected to reach staged on its own and the operator
// would benefit from being told to wait. Conservative: anything that isn't
// staged AND isn't past-staged counts as pending. (Note: a "released" order
// transitions to StatusInTransit per orders.Manager — there's no separate
// StatusReleased to filter out.)
func isPendingOrderStatus(s protocol.Status) bool {
	switch s {
	case orders.StatusInTransit, orders.StatusDelivered, orders.StatusConfirmed,
		orders.StatusCancelled, orders.StatusFailed:
		return false
	case orders.StatusStaged:
		return false
	default:
		return true
	}
}

func isNodeTaskTerminal(task *processes.NodeTask) bool {
	return domain.IsNodeTaskStateTerminal(task.State, task.Situation)
}

func ensureNodeTaskCanRequestOrder(orderID *int64, action string, db *store.DB) error {
	if orderID == nil {
		return nil
	}
	order, err := db.GetOrder(*orderID)
	if err != nil {
		return fmt.Errorf("%s already requested and order lookup failed: %w", action, err)
	}
	if !orders.IsTerminal(order.Status) {
		return fmt.Errorf("%s already requested with active order %s", action, order.UUID)
	}
	return nil
}
