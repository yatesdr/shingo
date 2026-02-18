package nodestate

import "time"

type NodeState struct {
	NodeID    int64
	NodeName  string
	NodeType  string
	Zone      string
	Capacity  int
	Enabled   bool
	Items     []InventoryItem
	ItemCount int
}

type InventoryItem struct {
	ID           int64     `json:"id"`
	MaterialID   int64     `json:"material_id"`
	MaterialCode string    `json:"material_code"`
	Quantity     float64   `json:"quantity"`
	IsPartial    bool      `json:"is_partial"`
	DeliveredAt  time.Time `json:"delivered_at"`
	Notes        string    `json:"notes,omitempty"`
	ClaimedBy    *int64    `json:"claimed_by,omitempty"`
}

type NodeMeta struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	NodeType string `json:"node_type"`
	Zone     string `json:"zone"`
	Capacity int    `json:"capacity"`
	Enabled  bool   `json:"enabled"`
}
