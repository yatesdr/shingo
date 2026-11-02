package bins

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
	"shingocore/store/loaders"
)

// KanbanLoopCTEs is a kanban's loop as CTEs, the one spelling the loop read
// (store.ListKanbanLoop) and the fence's above_pool (FencedNodesCTE) both render: kanban_tree,
// loop_place, carried, spoken and standing, for the kanbans a CTE scope(node_id) ahead of it
// names. kindParam is the placeholder carrying the kanban episode kind, which says whether a
// carried cart is its kanban's own ask (spoken.asked). See store/kanban_loop.go for what a loop
// is and why a cart a live order delivers into it is the loop's wherever it is.
//
// carried is the one place an order names its carts here: a live order's cart rows (order_bins,
// one per lift) whose drop step is a place of the loop, and a cart a live order names and holds
// no row for yet. A cart with two rows of one order (a relay) counts by its last lift. Both
// readers take it from this text.
func KanbanLoopCTEs(kindParam int) string {
	return `kanban_tree(kanban, id) AS (
	SELECT scope.node_id, c.id FROM scope JOIN nodes c ON c.parent_id = scope.node_id
	UNION ALL
	SELECT t.kanban, n2.id FROM nodes n2 JOIN kanban_tree t ON n2.parent_id = t.id
),
loop_place(kanban, node_id, place) AS (
	SELECT kanban, id, '` + LoopPlaceSlot + `' FROM kanban_tree
	UNION
	SELECT node_id, node_id, '` + LoopPlaceSlot + `' FROM scope
	UNION
	SELECT s.kanban_node_id, s.process_node_id, '` + LoopPlaceLine + `'
	  FROM kanban_supports s JOIN scope ON scope.node_id = s.kanban_node_id
	UNION
	SELECT g.id, h.position_node_id, '` + LoopPlaceWindow + `'
	  FROM bin_loaders l
	  JOIN nodes g ON g.name = l.outbound_dest
	  JOIN scope ON scope.node_id = g.id
	  JOIN bin_loader_homes h ON h.loader_id = l.id
	 WHERE l.archived_at IS NULL AND ` + loaders.ActsAsUnloaderSQL("l") + `
),
carried AS (
	SELECT lp.kanban, lp.place AS into_place, ob.bin_id AS cart, o.id AS order_id, COALESCE(ob.pick_step, -1) AS lift, o.origin_id
	  FROM order_bins ob
	  JOIN orders o ON o.id = ob.order_id
	  JOIN nodes dn ON dn.name = ` + helpers.StepNodeSQL("o", "ob.drop_step") + `
	  JOIN loop_place lp ON lp.node_id = dn.id
	 WHERE o.status IN (` + protocol.NonTerminalStatusSQLList() + `)
	UNION ALL
	SELECT lp.kanban, lp.place, o.named_bin_id, o.id, -1, o.origin_id
	  FROM orders o
	  JOIN nodes dn ON dn.name = o.delivery_node
	  JOIN loop_place lp ON lp.node_id = dn.id
	 WHERE o.status IN (` + protocol.NonTerminalStatusSQLList() + `) AND o.named_bin_id IS NOT NULL
	   AND NOT EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id)
),
spoken AS (
	SELECT DISTINCT ON (c.kanban, c.cart) c.kanban, c.cart, c.into_place,
	       EXISTS (SELECT 1 FROM demand_origins d
	                WHERE d.origin_id = c.origin_id AND d.kind = ` + fmt.Sprintf("$%d", kindParam) + ` AND ` + helpers.DemandOpenSQL("d") + `) AS asked
	  FROM carried c
	 ORDER BY c.kanban, c.cart, c.order_id DESC, c.lift DESC
),
standing AS (
	SELECT lp.kanban, b.id AS cart, lp.place
	  FROM bins b JOIN loop_place lp ON lp.node_id = b.node_id
	 WHERE b.status <> 'retired'
)`
}

// LoopPlaceSlot, LoopPlaceLine, LoopPlaceWindow and LoopPlaceFlight are where a cart of a
// kanban's loop can be (store.LoopCart.Place).
const (
	LoopPlaceSlot   = "slot"
	LoopPlaceLine   = "line"
	LoopPlaceWindow = "window"
	LoopPlaceFlight = "flight"
)
