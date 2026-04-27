package domain

import "time"

// Order is the edge-side material order. Distinct from
// shingocore/domain.Order: this row tracks the edge's view of a
// material movement (waybill, ETA, count confirmation, stage expiry)
// against a process node, and joins to Process/Station names for
// HMI rendering. The fleet/Core round trip writes back the
// VendorOrderID into WaybillID once Core dispatches.
type Order struct {
	ID             int64      `json:"id"`
	UUID           string     `json:"uuid"`
	OrderType      string     `json:"order_type"`
	Status         string     `json:"status"`
	ProcessNodeID  *int64     `json:"process_node_id,omitempty"`
	RetrieveEmpty  bool       `json:"retrieve_empty"`
	Quantity       int64      `json:"quantity"`
	DeliveryNode   string     `json:"delivery_node"`
	StagingNode    string     `json:"staging_node"`
	SourceNode     string     `json:"source_node"`
	LoadType       string     `json:"load_type"`
	WaybillID      *string    `json:"waybill_id"`
	ExternalRef    *string    `json:"external_ref"`
	FinalCount     *int64     `json:"final_count"`
	CountConfirmed bool       `json:"count_confirmed"`
	ETA            *string    `json:"eta"`
	AutoConfirm    bool       `json:"auto_confirm"`
	StagedExpireAt *time.Time `json:"staged_expire_at,omitempty"`
	PayloadCode    string     `json:"payload_code"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// Joined fields
	ProcessName     string `json:"process_name"`
	ProcessNodeName string `json:"process_node_name"`
	StationName     string `json:"station_name"`
}

// OrderHistory is one row in the edge order_history table — a status-
// change audit trail for a single edge Order. Note this differs from
// the core OrderHistory in that it captures both old and new status
// (the edge transition machine reports both directions).
type OrderHistory struct {
	ID        int64     `json:"id"`
	OrderID   int64     `json:"order_id"`
	OldStatus string    `json:"old_status"`
	NewStatus string    `json:"new_status"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}
