// operator_changeover_helpers.go — small predicates and guards shared
// across the changeover phase files (and operator_node_changeover.go).

package engine

import (
	"fmt"

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
