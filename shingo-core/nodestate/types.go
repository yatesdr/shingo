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
	ID        int64      `json:"id"`
	StyleID   int64      `json:"style_id"`
	StyleName string     `json:"style_name"`
	FormFactor string    `json:"form_factor"`
	Status     string    `json:"status"`
	DeliveredAt time.Time `json:"delivered_at"`
	Notes      string    `json:"notes,omitempty"`
	ClaimedBy  *int64    `json:"claimed_by,omitempty"`
}

