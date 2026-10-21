package helpers

import (
	"fmt"

	"shingo/protocol"
)

// ClearUUIDPrefix marks the parent of a dedicated loader home's clear
// (dispatch/clear_home.go): the job's row, whose delivery_node is the home.
// store/orders re-exports it; dispatch reads it from there.
const ClearUUIDPrefix = "core-clear-"

// LiveClearOnNodeSQL renders "a live home clear is running for the node whose
// name is nameExpr": a non-terminal clear parent delivering to it. ONE SPELLING
// for the clear's readers — the fence (a second clear of the home is
// refused), the exemption in the empty finders' loader arm (the empty on the
// home is sourceable while its clear runs) and the tests that pin both.
// Indexed: idx_orders_delivery_node.
func LiveClearOnNodeSQL(nameExpr string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM orders lc
		WHERE lc.delivery_node = %s AND lc.parent_order_id IS NULL
		  AND lc.edge_uuid LIKE '%s%%' AND lc.status NOT IN (%s))`,
		nameExpr, ClearUUIDPrefix, protocol.TerminalStatusSQLList())
}
