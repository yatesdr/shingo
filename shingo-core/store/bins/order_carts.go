package bins

import (
	"database/sql"
	"strings"
)

// order_carts.go — the bins reads an order's carts need at a block: which of
// the carts a lift names stands where the lift happened, and which of the carts
// a set-down names is in the robot's hands.

// atNodeSQL matches the bin's node (alias n) against a location as
// nodes.GetByDotName resolves it: a name with no dot whole, else the child
// named after the first dot under the parent named before it. $2 is the child,
// $3 the parent (” for a bare name).
const atNodeSQL = `n.name = $2 AND ($3 = '' OR n.parent_id = (SELECT id FROM nodes WHERE name = $3))`

func splitDotName(location string) (child, parent string) {
	if i := strings.Index(location, "."); i >= 0 {
		return location[i+1:], location[:i]
	}
	return location, ""
}

// StandingAt is the bins among ids recorded on location, in the order of ids
// (the caller's candidates, lowest pick step first). With no ids it is the bins
// order claimedBy has claimed there: a lift with no row of its order's at that
// node (an appended pickup). One statement.
func StandingAt(db *sql.DB, ids []int64, claimedBy int64, location string) ([]*Bin, error) {
	child, parent := splitDotName(strings.TrimSpace(location))
	rows, err := db.Query(BinJoinQuery+`
		WHERE `+atNodeSQL+`
		  AND (b.id = ANY($1::bigint[]) OR (cardinality($1::bigint[]) = 0 AND b.claimed_by = $4))
		ORDER BY array_position($1::bigint[], b.id), b.id`, ids, child, parent, claimedBy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}

// InHandAmong is the cart among ids the robot of order orderID holds now: claimed
// by the order and standing on _TRANSIT or a robot's carrier node — a fact about
// the plant, never stamped on a row. Of several, the one lifted last is the one
// in the forks (an earlier set-down whose block was lost leaves the other there
// too). ok=false when none is. One statement.
func InHandAmong(db *sql.DB, ids []int64, orderID int64) (int64, bool, error) {
	var id int64
	err := db.QueryRow(`SELECT b.id FROM bins b JOIN nodes n ON n.id = b.node_id
		WHERE b.id = ANY($1::bigint[]) AND b.claimed_by = $2
		  AND (n.name = '_TRANSIT' OR left(n.name, length($3)) = $3)
		ORDER BY b.last_lift_at DESC NULLS LAST, b.id DESC LIMIT 1`, ids, orderID, CarrierNodePrefix).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// OfOrder is the carts order orderID carries (its order_bins rows), read fresh,
// in the order its rows lift them. One statement.
func OfOrder(db *sql.DB, orderID int64) ([]*Bin, error) {
	rows, err := db.Query(BinJoinQuery+`
		WHERE b.id IN (SELECT ob.bin_id FROM order_bins ob WHERE ob.order_id = $1)
		ORDER BY (SELECT min(ob2.pick_step) FROM order_bins ob2 WHERE ob2.order_id = $1 AND ob2.bin_id = b.id) NULLS FIRST, b.id`,
		orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBins(rows)
}
