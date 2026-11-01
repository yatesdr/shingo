package store

import (
	"fmt"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// kanban_unloader_reads.go — the kanban's end of an unloader: ConsumeWindowsAt asks which
// group a window's cart goes back to, this asks which windows send carts back to a
// group. The surplus arm's reads (engine/kanban_surplus.go).

// SurplusEdgeUUIDPrefix marks the surplus arm's own moves, readably, on the row: a
// resident of a kanban on its way to a window of the kanban's unloader, or to a source
// of the kanban. Dispatch issues them and the engine confirms a landed one
// (TestKanbanPin_LandedReceiptConfirmsThreePrefixesOnly).
const SurplusEdgeUUIDPrefix = "core-sp-"

// KanbanWindow is one window of an active unloader whose outbound is the kanban.
type KanbanWindow struct {
	LoaderID int64
	// Shared says the loader's layout has shared windows, the only layout a part-less
	// cart is delivered to (loaders.DeliveryTargets), and Funnel that it takes one
	// window at a time.
	Shared, Funnel bool
	Node           *nodes.Node
	// Row is the cart as a window's walk reads it (kanban.Assign): the bin
	// standing there, or the cart a surplus move is bringing to it, Occupied for both.
	Row protocol.NodeBinsRow
	// Floor is the cart's floor, Sized false for a type with none entered.
	Floor domain.FloorSize
	Sized bool
	// Surplus says the cart is the arm's: its move is on the way, or it landed and
	// nothing has moved the cart since. Moving says the move is still live.
	Surplus, Moving bool
}

// kanbanWindowsSQL is one kanban's windows in the plant's order, each with what stands
// on it. A window's arm order is the newest naming it: a live one, or one landed whose
// cart still stands there and has had no order since (a CLEAR's empty-out names the
// cart, so it ends the arm's claim on it).
var kanbanWindowsSQL = `SELECT ` + nodes.SelectCols + `, l.id, l.layout, l.funnel_windows,
		COALESCE(b.id, 0), COALESCE(b.label, ''), COALESCE(bt.code, ''), COALESCE(bt.bare, false),
		COALESCE(b.payload_code, ''), b.totes_on_cart, bt.totes_per_layer,
		COALESCE(bt.width_in, 0), COALESCE(bt.length_in, 0),
		COALESCE(arm.id IS NOT NULL AND b.id = arm.cart, false),
		COALESCE(arm.id IS NOT NULL AND b.id = arm.cart AND arm.live, false)
	` + nodes.FromClause + `
	JOIN bin_loader_homes h ON h.position_node_id = n.id
	JOIN bin_loaders l ON l.id = h.loader_id
	JOIN nodes g ON g.name = l.outbound_dest
	LEFT JOIN LATERAL (
		SELECT o.id, COALESCE(o.bin_id, o.named_bin_id) AS cart,
		       o.status NOT IN (` + protocol.TerminalStatusSQLList() + `) AS live
		  FROM orders o
		 WHERE o.delivery_node = n.name AND o.edge_uuid LIKE $2
		   AND (o.status NOT IN (` + protocol.TerminalStatusSQLList() + `)
		        OR (o.status = '` + string(protocol.StatusConfirmed) + `'
		            AND EXISTS (SELECT 1 FROM bins lb WHERE lb.id = COALESCE(o.bin_id, o.named_bin_id) AND lb.node_id = n.id)
		            AND NOT EXISTS (SELECT 1 FROM orders later
		                             WHERE COALESCE(later.bin_id, later.named_bin_id) = COALESCE(o.bin_id, o.named_bin_id)
		                               AND later.id > o.id)))
		 ORDER BY o.id DESC LIMIT 1
	) arm ON TRUE
	LEFT JOIN LATERAL (SELECT sb.id FROM bins sb WHERE sb.node_id = n.id ORDER BY sb.id LIMIT 1) here ON TRUE
	LEFT JOIN bins b ON b.id = COALESCE(here.id, arm.cart)
	LEFT JOIN bin_types bt ON bt.id = b.bin_type_id
	WHERE g.id = $1 AND l.archived_at IS NULL AND ` + loaders.ActsAsUnloaderSQL("l") + `
	ORDER BY l.id, h.sort_order, h.position_node_id`

// ListKanbanWindows reads a kanban's windows and their carts, in one statement. Only a
// short kanban pays it (Maintainer.tickKanban): a steady tick reads none. Pin:
// TestKanbanCost_KeeperTick.
func (db *DB) ListKanbanWindows(kanbanID int64) ([]KanbanWindow, error) {
	rows, err := db.DB.Query(kanbanWindowsSQL, kanbanID, SurplusEdgeUUIDPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("kanban windows %d: %w", kanbanID, err)
	}
	defer rows.Close()
	var out []KanbanWindow
	for rows.Next() {
		var (
			w             KanbanWindow
			layout        string
			width, length float64
		)
		n, err := nodes.ScanDoorLine(rows, &w.LoaderID, &layout, &w.Funnel,
			&w.Row.BinID, &w.Row.BinLabel, &w.Row.BinTypeCode, &w.Row.Bare,
			&w.Row.PayloadCode, &w.Row.TotesOnCart, &w.Row.TotesPerLayer,
			&width, &length, &w.Surplus, &w.Moving)
		if err != nil {
			return nil, fmt.Errorf("kanban windows %d: scan: %w", kanbanID, err)
		}
		w.Node, w.Shared = n, layout == loaders.LayoutSharedWindow
		w.Row.Node, w.Row.Occupied = n.Name, w.Row.BinID != 0
		w.Floor, w.Sized = domain.BinType{WidthIn: width, LengthIn: length}.FloorSize()
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListKanbanSurplus reads the carts of the named types the surplus arm may move out of
// a kanban (bins.ListKanbanSurplus): oldest first.
func (db *DB) ListKanbanSurplus(kanbanID int64, codes []string) ([]*bins.Bin, error) {
	return bins.ListKanbanSurplus(db.DB, kanbanID, codes)
}

// CountSurplusLeaving counts the arm's live moves lifting from a kanban's positions.
// One statement, and only when the arm is about to send a cart to a source.
func (db *DB) CountSurplusLeaving(kanbanID int64) (int, error) {
	var n int
	err := db.DB.QueryRow(`SELECT COUNT(*) FROM orders o
		JOIN nodes sn ON sn.name = o.source_node
		WHERE sn.parent_id = $1 AND o.edge_uuid LIKE $2
		  AND o.status NOT IN (`+protocol.TerminalStatusSQLList()+`)`, kanbanID, SurplusEdgeUUIDPrefix+"%").Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("surplus leaving %d: %w", kanbanID, err)
	}
	return n, nil
}
