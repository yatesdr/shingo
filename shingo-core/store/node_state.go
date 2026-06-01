package store

import "log"

// NodeState represents the current state of a node with its bin contents.
type NodeState struct {
	NodeID    int64
	NodeName  string
	Zone      string
	Enabled   bool
	Items     []BinItem
	ItemCount int
	// ContentsUnknown is set when the node's bins could not be read (a transient
	// DB error). The node is still included in the snapshot — flagged rather than
	// silently dropped — so a consumer can tell "no bins" from "couldn't read".
	ContentsUnknown bool
}

// BinItem describes a bin at a node for state queries.
type BinItem struct {
	ID                int64  `json:"id"`
	PayloadCode       string `json:"payload_code"`
	Label             string `json:"label"`
	ManifestConfirmed bool   `json:"manifest_confirmed"`
	UOPRemaining      int    `json:"uop_remaining"`
	ClaimedBy         *int64 `json:"claimed_by,omitempty"`
}

// ListNodeStates returns the state of all nodes with their bin contents.
func (db *DB) ListNodeStates() (map[int64]*NodeState, error) {
	nodes, err := db.ListNodes()
	if err != nil {
		return nil, err
	}
	states := make(map[int64]*NodeState, len(nodes))
	for _, node := range nodes {
		bins, err := db.ListBinsByNode(node.ID)
		if err != nil {
			// Don't drop the node from the snapshot on a transient read error — a
			// missing node is harder to diagnose than one flagged unreadable.
			log.Printf("store: list bins for node %d: %v", node.ID, err)
			states[node.ID] = &NodeState{
				NodeID:          node.ID,
				NodeName:        node.Name,
				Zone:            node.Zone,
				Enabled:         node.Enabled,
				ContentsUnknown: true,
			}
			continue
		}
		items := make([]BinItem, len(bins))
		for i, b := range bins {
			items[i] = BinItem{
				ID:                b.ID,
				PayloadCode:       b.PayloadCode,
				Label:             b.Label,
				ManifestConfirmed: b.ManifestConfirmed,
				UOPRemaining:      b.UOPRemaining,
				ClaimedBy:         b.ClaimedBy,
			}
		}
		states[node.ID] = &NodeState{
			NodeID:    node.ID,
			NodeName:  node.Name,
			Zone:      node.Zone,
			Enabled:   node.Enabled,
			Items:     items,
			ItemCount: len(items),
		}
	}
	return states, nil
}
