package bins

import (
	"database/sql"
	"fmt"

	"shingocore/store/internal/helpers"
	"shingocore/store/internal/nodetree"
	"shingocore/store/reservations"
)

// drain.go — the carts an unloader's drain boxes may pull out of its inbound
// source (W14; dispatch.DrainLoader names each one it moves).

// ListDrainableInGroup returns the carts in an unloader's inbound source whose
// carrier state is one of ticked (loaders.DrainStates.Ticked), best first: the
// population the drain's windows are filled from, one statement.
//
// THE EMPTY FINDERS' OWN WHERE. EmptyCarrierWhereInGroup, asked as no
// destination (so no loader's buffers are exempted and no cleared home is
// offered): sourceable and not staged, unheld, at a live node, no part, on no
// claim's own position and no live loader position (so a cart already on any
// unloader's window is never drained off it), not bare, not on quality hold. A
// bare marker is never drained: it is no ordinary empty, and stage 2's own pull
// (ListBareInGroup) is the only one that moves it. To that:
//
//   - the state, helpers.CarrierStateSQL = ANY(ticked), the SQL twin the
//     agreement table pins equal to protocol.CarrierStateOf
//     (TestCarrierState_GoSQLAgree), so each box pulls its state and only its
//     state;
//   - reachable only (helpers.ReachableSQL): the drain names its bin, so a
//     buried cart would be a dig nobody planned, and the drain waits for the
//     lane mouth to clear instead;
//   - not in a lane a dig holds (NotForeignDugArm, asked as nobody's dig);
//   - across the source node itself and its subtree, but never inside a group
//     that declares a level below it (nodetree.UnmaintainedDescendantsOf): an
//     unloader never drains a maintained group (owner, 2026-10-22). Whether the
//     source itself declares one is the caller's re-check (store.DeclaresLevel).
//
// NOT ListBareInGroup's clauses: those admit `staged` and exclude no loader or
// line position. Ranked as every empty is ranked, by accessibleEmptyTerms
// (accessible, shallowest, id; the first term is true for every row here), with
// no LIMIT: the caller walks the list, one cart per free window that admits it.
// An empty ticked set reads nothing.
func ListDrainableInGroup(db *sql.DB, groupNodeID int64, ticked []string) ([]*Bin, error) {
	if len(ticked) == 0 {
		return nil, nil
	}
	a := &emptyQueryArgs{vals: []any{groupNodeID, int64(0), ticked}}
	q := nodetree.UnmaintainedDescendantsOf(1) + " " + BinJoinQuery + EmptyCarrierWhereInGroup(2, 1) + `
	  AND (b.node_id = $1 OR b.node_id IN (SELECT id FROM unmaintained))
	  AND ` + helpers.CarrierStateSQL("b", "bt") + ` = ANY($3)
	  AND (n.parent_id IS NULL OR n.depth IS NULL OR ` + helpers.ReachableSQL("n") + `)` +
		NotForeignDugArm(a.add(string(reservations.ModeDig)),
			a.add(reservations.Anyone.OrderID), a.add(reservations.Anyone.LaneOwner)) + `
	ORDER BY ` + accessibleEmptyTerms
	rows, err := db.Query(q, a.vals...)
	if err != nil {
		return nil, fmt.Errorf("list drainable carts in group %d: %w", groupNodeID, err)
	}
	defer rows.Close()
	return scanBins(rows)
}
