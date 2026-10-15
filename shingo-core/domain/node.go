package domain

import "time"

// TransitNodeName is the well-known name of the synthetic node that
// holds bins while they are physically in transit between their source
// and destination. Created by migration v15. Bins occupying this node
// have `is_synthetic=true` on the node row, which the synthetic half of the
// shared sourcing predicate (helpers.BinAtLiveNodeSQL) excludes for every
// reader that composes it — so in-flight bins never get re-claimed by
// another order.
const TransitNodeName = "_TRANSIT"

// Node is any addressable location in the facility graph — physical
// storage slots, lanes, group-level aggregates, and synthetic routing
// parents. The node graph is a forest keyed by ParentID; Depth is the
// tree depth from the nearest root and is maintained by the store
// layer.
//
// The two Joined fields (NodeTypeCode, ParentName)
// are populated by every SELECT in store/nodes via LEFT JOINs on
// node_types and a self-join on nodes. They live here because every
// rendering path uses at least one of them and materialising them into
// the struct keeps call sites flat.
type Node struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// DisplayName is the name a PERSON reads, editable on the Nodes page.
	// The node's own Name is NOT editable from shingo, ever: it is the
	// fleet's identity - scenesync matches the fleet's location set against
	// nodes by name, and a physical node whose name the current scene does
	// not carry is DELETED on the next sync, stranding every claim, order
	// and loader config that keyed on it. So the cosmetic rename rides a
	// column of its own: empty (the default) means "render Name as always",
	// and setting it changes what screens say without touching what the
	// fleet knows.
	DisplayName string    `json:"display_name"`
	IsSynthetic bool      `json:"is_synthetic"`
	Zone        string    `json:"zone"`
	Enabled     bool      `json:"enabled"`
	Depth       *int      `json:"depth,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	NodeTypeID  *int64    `json:"node_type_id,omitempty"`
	ParentID    *int64    `json:"parent_id,omitempty"`
	// ClaimedBy is the order that has atomically claimed this node as a
	// destination slot (the store dual of bins.claimed_by); nil when
	// unclaimed. Slot selection skips claimed slots; the dispatch-time CAS
	// (ClaimSlot) is the authority that prevents two orders dispatching into
	// the same slot.
	ClaimedBy *int64 `json:"claimed_by,omitempty"`
	// Joined fields
	NodeTypeCode string `json:"node_type_code,omitempty"`
	ParentName   string `json:"parent_name,omitempty"`
}

// RenderName is what a screen shows for this node: the display name when
// someone has set one, the fleet's name otherwise. One spelling, so every
// surface that renders a node says the same thing about it.
func (n *Node) RenderName() string {
	if n.DisplayName != "" {
		return n.DisplayName
	}
	return n.Name
}
