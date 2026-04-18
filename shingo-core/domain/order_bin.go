package domain

import "time"

// OrderBin is a junction row tying a complex order to one of its
// claimed bins at a given step — the step index (0-based), the
// fleet-side action for that step, and both the current and
// destination node names so the dispatcher can stage intermediate
// moves without re-resolving. Simple orders (single bin) use
// Order.BinID directly and do not produce OrderBin rows.
type OrderBin struct {
	ID        int64     `json:"id"`
	OrderID   int64     `json:"order_id"`
	BinID     int64     `json:"bin_id"`
	StepIndex int       `json:"step_index"`
	Action    string    `json:"action"`
	NodeName  string    `json:"node_name"`
	DestNode  string    `json:"dest_node"`
	CreatedAt time.Time `json:"created_at"`
}
