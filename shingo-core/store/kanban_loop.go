package store

import (
	"fmt"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
	"shingocore/store/loaders"
)

// kanban_loop.go — the one read of where a kanban's carts are.
//
// A LOOP is the kanban's slots, the positions it supports, the homes of each unloader whose
// outbound is the kanban, and the flights between them. A lift writes bins.node_id to
// _TRANSIT or a robot's _ROBOT:<v> (bins.LiftToTransitTx), so a place alone loses every cart
// on a robot: a cart a live order is delivering into the loop is the loop's wherever it is.
//
// SPOKEN-FOR is the one predicate. A cart a live order delivers into the loop is spoken for,
// and the order is what says where it counts: a standing cart with a filed empty-out counts
// once, as coming. Everything else standing on a place of the loop counts there. A keeper's
// own ask carries its cart too, and is counted as asked (ListOpenKeeperEpisodes), never here
// a second time.
//
// AN ORDER'S CARTS ARE ITS ROWS (order_bins, one per lift): a cart is carried into the loop by
// a live order's row whose drop step is a place of it, and by a cart a live order names and holds
// no row for yet. A cart with two rows of one order (a relay) counts by its last lift.
//
// A CART ON A ROBOT WITH NO LIVE ORDER counts for the kanban its newest row named, its own pick or
// drop, read as unowned. Recovering it is the anomaly surface's.
//
// ONE STATEMENT, so a cart landing mid-read cannot count in two terms or in none. One row a
// cart, whatever the plant's size: the keeper's tick and a snapshot build read this and no
// more about the carts.
const (
	// LoopPlaceSlot, LoopPlaceLine, LoopPlaceWindow and LoopPlaceFlight are where a cart of
	// the loop can be.
	LoopPlaceSlot   = "slot"
	LoopPlaceLine   = "line"
	LoopPlaceWindow = "window"
	LoopPlaceFlight = "flight"
)

// LoopCart is one cart of one kanban's loop.
type LoopCart struct {
	Kanban int64
	BinID  int64
	Label  string
	// Code and FamilyID are the cart's bin type and family; a bare cart reads its marker's
	// code and its carrier's family.
	Code       string
	FamilyID   int64
	FamilyCode string
	// Place is where the cart is, Node the node it stands on (blank in flight). Into is where
	// its live order delivers it, blank for a cart not spoken for.
	Place, Node, Into string
	// Asked says the order carrying it is the kanban's own open ask.
	Asked bool
	// Unowned is a cart on a robot with no live order.
	Unowned bool
	// Resident says the finder's own WHERE takes it as an empty standing in the kanban.
	Resident bool
	// Stock says the cart is at no height said, which leaves tops & bottoms out.
	Stock         bool
	State         protocol.CarrierState
	TotesOnCart   *int
	TotesPerLayer *int
	Bare          bool
	PayloadCode   string
}

// Named debt: bin_loaders.outbound_dest and a claim's positions join nodes by name here, as they
// do everywhere, and ids for them are a change to the loader aggregate (a rename warns:
// service.NodeService.RenameWarnings).
// kanbanLoopSQL is the read; see the file's head. $1 the kanbans (NULL: all), $2 the kanban
// episode kind.
var kanbanLoopSQL = `
WITH RECURSIVE scope AS (
	SELECT k.node_id FROM kanbans k WHERE $1::bigint[] IS NULL OR k.node_id = ANY($1::bigint[])
),
kanban_tree(kanban, id) AS (
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
	                WHERE d.origin_id = c.origin_id AND d.kind = $2 AND ` + helpers.DemandOpenSQL("d") + `) AS asked
	  FROM carried c
	 ORDER BY c.kanban, c.cart, c.order_id DESC, c.lift DESC
),
standing AS (
	SELECT lp.kanban, b.id AS cart, lp.place
	  FROM bins b JOIN loop_place lp ON lp.node_id = b.node_id
	 WHERE b.status <> 'retired'
),
in_flight_no_order AS (
	SELECT DISTINCT lp.kanban, b.id AS cart
	  FROM bins b
	  JOIN nodes n ON n.id = b.node_id
	  JOIN LATERAL (
		SELECT ` + helpers.StepNodeSQL("o", "ob.drop_step") + ` AS drop_node, ` + helpers.StepNodeSQL("o", "ob.pick_step") + ` AS pick_node
		  FROM order_bins ob JOIN orders o ON o.id = ob.order_id
		 WHERE ob.bin_id = b.id ORDER BY o.id DESC, ob.pick_step DESC NULLS LAST LIMIT 1
	  ) last ON TRUE
	  JOIN nodes ln ON ln.name IN (last.drop_node, last.pick_node)
	  JOIN loop_place lp ON lp.node_id = ln.id
	 WHERE (n.name = '` + domain.TransitNodeName + `' OR ` + bins.CarrierNodeSQL + `)
	   AND NOT EXISTS (SELECT 1 FROM order_bins leg JOIN orders live ON live.id = leg.order_id
	                    WHERE leg.bin_id = b.id
	                      AND live.status IN (` + protocol.NonTerminalStatusSQLList() + `))
),
loop_cart AS (
	SELECT s.kanban, s.cart, COALESCE(st.place, '` + LoopPlaceFlight + `') AS place, s.into_place, s.asked, false AS unowned
	  FROM spoken s LEFT JOIN standing st ON st.kanban = s.kanban AND st.cart = s.cart
	UNION ALL
	SELECT st.kanban, st.cart, st.place, '', false, false
	  FROM standing st LEFT JOIN spoken s ON s.kanban = st.kanban AND s.cart = st.cart
	 WHERE s.cart IS NULL
	UNION ALL
	SELECT kanban, cart, '` + LoopPlaceFlight + `', '', false, true FROM in_flight_no_order
),
residents AS (
	SELECT kt.kanban, b.id AS cart ` + bins.BinFromClause + `
	  JOIN kanban_tree kt ON kt.id = b.node_id` + bins.EmptyCarrierWhereForKanban("kt.kanban") + `
)
SELECT lc.kanban, b.id, b.label, bt.code, bt.family_id, f.code, lc.place, COALESCE(n.name, ''), lc.into_place, lc.asked, lc.unowned,
       r.cart IS NOT NULL, ` + bins.NoHeightSaidSQL() + `, ` + helpers.CarrierStateSQL("b", "bt") + `,
       b.totes_on_cart, bt.totes_per_layer, bt.bare, COALESCE(b.payload_code, '')
  FROM loop_cart lc
  JOIN bins b ON b.id = lc.cart
  JOIN bin_types bt ON bt.id = b.bin_type_id
  JOIN carrier_families f ON f.id = bt.family_id
  LEFT JOIN nodes n ON n.id = b.node_id
  LEFT JOIN residents r ON r.kanban = lc.kanban AND r.cart = lc.cart
 ORDER BY lc.kanban, b.id`

// ListKanbanLoop reads the carts of the named kanbans' loops (nil: all), in one statement.
func (db *DB) ListKanbanLoop(ids []int64) ([]LoopCart, error) {
	var arg any
	if ids != nil {
		arg = ids
	}
	rows, err := db.DB.Query(kanbanLoopSQL, arg, protocol.EpisodeKindKanban)
	if err != nil {
		return nil, fmt.Errorf("kanban loop: %w", err)
	}
	defer rows.Close()
	var out []LoopCart
	for rows.Next() {
		var c LoopCart
		if err := rows.Scan(&c.Kanban, &c.BinID, &c.Label, &c.Code, &c.FamilyID, &c.FamilyCode, &c.Place, &c.Node, &c.Into, &c.Asked, &c.Unowned,
			&c.Resident, &c.Stock, &c.State, &c.TotesOnCart, &c.TotesPerLayer, &c.Bare, &c.PayloadCode); err != nil {
			return nil, fmt.Errorf("kanban loop: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
