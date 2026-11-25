package helpers

import "shingocore/domain"

// OrderTripSQL is domain.Trip asked of the bins, for a set of orders: one row
// per order of $1 (a bigint[]) that exists, as (id, carried, set_down). It is
// the one SQL spelling; the half an order's row answers without a read is
// domain.Order.TripFromRow, and store.DB.OrderTrips composes the two.
//
//   - carried: a bin the order claims stands at _TRANSIT. A robot has lifted
//     it and not set it down; the lift itself is not audited, so the bin's
//     position is the fact.
//   - set_down: the order carries a cart with a drop, and every such cart
//     stands on its final drop's node with the order's claim on it gone. The
//     final drop is the cart's highest drop step (domain.Order.FinalDrops);
//     its node is the plan's (StepNodeSQL), resolved as nodes.GetByDotName
//     resolves a name, which is how the landing finds where to put the cart
//     (engine settleCarts); the landing is a handoff and releases the claim
//     (store.ApplyMultiBinArrival). A relay's set-down before the final drop
//     is not set down, and keeps its claim besides.
//
// A cart whose bin row is gone is not set down. One statement whatever the
// size of the set; the plan is parsed once per cart (StepNodeSQL's COST). A
// variable only because it is composed from StepNodeSQL; nothing assigns it.
var OrderTripSQL = `
SELECT o.id,
	EXISTS (SELECT 1 FROM bins b
	         WHERE b.claimed_by = o.id
	           AND b.node_id IN (SELECT id FROM nodes WHERE name = '` + domain.TransitNodeName + `')),
	EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id AND ob.drop_step IS NOT NULL)
	AND NOT EXISTS (
		SELECT 1
		  FROM (SELECT DISTINCT ON (ob.bin_id) ob.bin_id, ob.drop_step
		          FROM order_bins ob
		         WHERE ob.order_id = o.id AND ob.drop_step IS NOT NULL
		         ORDER BY ob.bin_id, ob.drop_step DESC) fd
		  LEFT JOIN bins b ON b.id = fd.bin_id
		  CROSS JOIN LATERAL (SELECT ` + StepNodeSQL("o", "fd.drop_step") + ` AS name) dn
		 WHERE b.claimed_by = o.id
		    OR NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = b.node_id AND ` + atDotNameSQL("n", "dn.name") + `))
FROM orders o
WHERE o.id = ANY($1::bigint[])`

// atDotNameSQL is true when node n is the node name (an SQL text expression)
// names, as nodes.GetByDotName resolves it: a name with no dot, whole; else
// the child named after the first dot under a parent named before it. Node
// references are bare or dotted depending on the planner that wrote them.
func atDotNameSQL(n, name string) string {
	return `(CASE WHEN strpos(` + name + `, '.') = 0 THEN ` + n + `.name = ` + name + `
		ELSE ` + n + `.name = substr(` + name + `, strpos(` + name + `, '.') + 1)
		 AND ` + n + `.parent_id IN (SELECT p.id FROM nodes p WHERE p.name = split_part(` + name + `, '.', 1)) END)`
}
