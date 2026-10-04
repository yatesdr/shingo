package store

import (
	"fmt"

	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// LoaderPlacementMember is one member of a loader as loader placement reads it:
// the member row, its node's name, and whether another order holds the slot.
type LoaderPlacementMember struct {
	loaders.Home
	NodeName string
	// HeldByStranger: an order other than the one being placed holds a hard
	// claim or an active slot reservation on the member's node
	// (nodes.SlotHeldByStrangerSQL).
	HeldByStranger bool
}

// LoaderMembersForPlacement returns every member of the loader that owns
// positionNodeID, in ListHomes order, each with its node's name and whether an
// order other than owner holds the slot. Empty when the node is no loader's
// member.
//
// ONE STATEMENT FOR THE WHOLE LOADER, so loader placement reads the hold on its
// home and on every buffer once per placement, never once per candidate. It
// stands in for the member-row lookup placement already made
// (GetLoaderHomeByPositionNode), and the node name it carries stands in for the
// per-member node read the buffer walk made.
//
// It lives here rather than in store/loaders because it joins two aggregates:
// the loader's members and the nodes' holds.
func (db *DB) LoaderMembersForPlacement(positionNodeID, owner int64) ([]LoaderPlacementMember, error) {
	rows, err := db.Query(`
		SELECT h.loader_id, h.position_node_id, h.payload_code, h.home_kind, h.uop_threshold, h.sort_order,
		       n.name, `+nodes.SlotHeldByStrangerSQL("n", "$2")+`
		FROM bin_loader_homes h
		JOIN nodes n ON n.id = h.position_node_id
		WHERE h.loader_id = (SELECT owner_row.loader_id FROM bin_loader_homes owner_row
		                     WHERE owner_row.position_node_id = $1)
		ORDER BY h.sort_order, h.position_node_id`, positionNodeID, owner)
	if err != nil {
		return nil, fmt.Errorf("placement members of the loader at node %d: %w", positionNodeID, err)
	}
	defer rows.Close()
	var out []LoaderPlacementMember
	for rows.Next() {
		var m LoaderPlacementMember
		if err := rows.Scan(&m.LoaderID, &m.PositionNodeID, &m.PayloadCode, &m.Kind, &m.UOPThreshold, &m.SortOrder,
			&m.NodeName, &m.HeldByStranger); err != nil {
			return nil, fmt.Errorf("scan placement member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
