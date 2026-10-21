package orders

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
)

// clear.go — the reads a dedicated loader home's clear needs
// (dispatch/clear_home.go). The clear is a compound parent whose delivery node
// is the home, marked by ClearUUIDPrefix; its children are plain moves.

// ClearUUIDPrefix marks a home clear's parent row — see helpers.ClearUUIDPrefix.
const ClearUUIDPrefix = helpers.ClearUUIDPrefix

// LiveClearForHome reports whether a clear of the home named homeName is
// running: the fence. One statement on idx_orders_delivery_node.
func LiveClearForHome(db *sql.DB, homeName string) (bool, error) {
	var live bool
	err := db.QueryRow(`SELECT `+helpers.LiveClearOnNodeSQL("$1"), homeName).Scan(&live)
	return live, err
}

// ClearPullCandidates lists the acquiring empty pulls whose own finders could
// see a carrier on the home: a retrieve_empty that is not for the home itself,
// sourcing plant-wide (no source), from a group the home stands in (any
// ancestor, bare or dotted name), or from a position of the home's loader (the
// dedicated pool), or DELIVERING to a position of the home's loader, whatever
// it sources from. The last is the loader's own demand — the replenishment
// loop's core-l1- ask for another home, an operator's request there — and its
// group finder sees a cleared home of its own loader wherever the home stands
// (bins.InGroupArm). Whether it takes the carrier is its finder's answer, not
// this read's. In the plant's demand order. One statement.
func ClearPullCandidates(db *sql.DB, homeID int64, homeName string, loaderPositions []string) ([]int64, error) {
	rows, err := db.Query(fmt.Sprintf(`WITH RECURSIVE up(id, parent_id, name) AS (
			SELECT id, parent_id, name FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id, n.name FROM nodes n JOIN up ON n.id = up.parent_id)
		SELECT o.id FROM orders o
		WHERE o.order_type = $2 AND o.parent_order_id IS NULL AND o.status IN (%s)
		  AND o.delivery_node <> $3
		  AND (COALESCE(o.source_node, '') = ''
		       OR o.source_node IN (SELECT name FROM up WHERE id <> $1)
		       OR split_part(o.source_node, '.', 2) IN (SELECT name FROM up WHERE id <> $1)
		       OR o.source_node = ANY($4::text[])
		       OR o.delivery_node = ANY($4::text[]))
		ORDER BY %s`, protocol.AcquiringStatusSQLList(), DemandRankOrderBySQL()),
		homeID, string(protocol.OrderTypeRetrieveEmpty), homeName, loaderPositions)
	if err != nil {
		return nil, fmt.Errorf("list clear pull candidates: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
