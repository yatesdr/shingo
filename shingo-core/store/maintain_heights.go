package store

import (
	"database/sql"
	"fmt"

	"shingocore/store/bins"
	"shingocore/store/internal/nodetree"
)

// maintain_heights.go — a level type's residents by the totes on each cart,
// for an unloader's group line (engine.UnloaderGroupLines), which counts a cart
// toward a wanted height only on an exact multiple of its type's totes per
// layer. Its comings need no read of their own: the keeper's coming is
// orders.ListTypedInboundToGroup's rows, which carry each cart's number.
//
// THE KEEPER'S POPULATION, SLICED: the keeper's resident count (countLevel)
// grouped by bins.totes_on_cart, composing the keeper's WHERE verbatim. A cart
// with no number is in no bucket, and the keeper's total stays the only total.
// Pin: TestCountsByTotes_AgreeWithTheKeepersCounts.

// CountEmptyBinsOfTypeInGroupByTotes counts what CountEmptyBinsOfTypeInGroup
// counts, by totes_on_cart: bins.EmptyOfTypeInGroupWhere, interpolated as the
// count does, with the same three arguments. Carts with no number are left
// out.
func (db *DB) CountEmptyBinsOfTypeInGroupByTotes(binTypeCode string, groupNodeID int64) (map[int]int, error) {
	if binTypeCode == "" {
		return map[int]int{}, nil
	}
	rows, err := db.Query(
		nodetree.DescendantsOf(2)+" SELECT b.totes_on_cart, COUNT(*) "+bins.BinFromClause+
			bins.EmptyOfTypeInGroupWhere+`
		  AND b.totes_on_cart IS NOT NULL
		GROUP BY b.totes_on_cart`,
		binTypeCode, groupNodeID, 0)
	if err != nil {
		return nil, fmt.Errorf("count empty %s in group %d by totes: %w", binTypeCode, groupNodeID, err)
	}
	return scanCountsByTotes(rows)
}

func scanCountsByTotes(rows *sql.Rows) (map[int]int, error) {
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var totes, n int
		if err := rows.Scan(&totes, &n); err != nil {
			return nil, fmt.Errorf("scan count by totes: %w", err)
		}
		out[totes] = n
	}
	return out, rows.Err()
}
