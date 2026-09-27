package store

import (
	"database/sql"
	"errors"
	"fmt"

	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// PlannedPickStands answers whether a birth pick (orders.plan, v147) still
// stands for the order asking, and returns the node the bin stands on when it
// does. It asks what the pick asked of this bin and what the reservation's own
// CAS (reservations.Acquire) will ask next: the bin is at the planned node, the
// node is live, the bin is unclaimed, unlocked and promised to nobody, its lane
// is not dug by another order, and its payload, status and manifest flag are
// what the pick saw. A lane buried since the pick is not asked here: the lane
// admission after the reserve parks it, as it parks a found bin.
//
// (nil, nil) is "stale": the caller picks again. Asking the reservation's
// question here too is what keeps a stale plan from parking on a bin somebody
// else holds, pass after pass, where a pick would have taken another bin.
// One statement, where a pick is tens.
func (db *DB) PlannedPickStands(p *domain.PlannedPick, asker domain.DigAsker) (*nodes.Node, error) {
	if p == nil {
		return nil, nil
	}
	args := append([]any{p.BinID, p.Node, p.Payload, p.Status, p.Confirmed, string(reservations.ModeDig)}, asker.Args()...)
	row := db.QueryRow(`SELECT `+nodes.SelectCols+` `+nodes.FromClause+`
		JOIN bins b ON b.node_id = n.id
		WHERE b.id = $1 AND n.name = $2
		  AND `+bins.BinAtLiveNodeSQL+`
		  AND `+bins.BinUnheldSQL+`
		  AND COALESCE(b.payload_code, '') = $3
		  AND b.status = $4
		  AND b.manifest_confirmed = $5`+
		bins.NotForeignDugArm(6, 7, 8),
		args...)
	n, err := nodes.ScanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("planned pick for bin %d at %s: %w", p.BinID, p.Node, err)
	}
	return n, nil
}
