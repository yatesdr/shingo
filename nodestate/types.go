package nodestate

import "time"

type NodeState struct {
	NodeID    int64
	NodeName  string
	NodeType  string
	Zone      string
	Capacity  int
	Enabled   bool
	Items     []PayloadItem
	ItemCount int
}

type PayloadItem struct {
	ID              int64      `json:"id"`
	PayloadTypeID   int64      `json:"payload_type_id"`
	PayloadTypeName string     `json:"payload_type_name"`
	FormFactor      string     `json:"form_factor"`
	Status          string     `json:"status"`
	DeliveredAt     time.Time  `json:"delivered_at"`
	Notes           string     `json:"notes,omitempty"`
	ClaimedBy       *int64     `json:"claimed_by,omitempty"`
}

type NodeMeta struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	NodeType string `json:"node_type"`
	Zone     string `json:"zone"`
	Capacity int    `json:"capacity"`
	Enabled  bool   `json:"enabled"`
}
