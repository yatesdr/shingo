package domain

import (
	"time"

	"shingo/protocol"
)

// Order is the edge-side material order. Distinct from
// shingocore/domain.Order: this row tracks the edge's view of a
// material movement (waybill, ETA, count confirmation, stage expiry)
// against a process node, and joins to Process/Station names for
// HMI rendering. The fleet/Core round trip writes back the
// VendorOrderID into WaybillID once Core dispatches.
type Order struct {
	ID             int64      `json:"id"`
	UUID           string     `json:"uuid"`
	OrderType      protocol.OrderType `json:"order_type"`
	Status         protocol.Status `json:"status"`
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
	// BinUOPRemaining is the bin's uop_remaining at delivery time, snapshot
	// from Core via the OrderDelivered envelope (see protocol.OrderDelivered).
	// handleNormalReplenishment uses this to reset lineside UOP from the
	// bin's actual contents instead of guessing claim.UOPCapacity. Nil for
	// multi-bin orders, for older Core builds, and before the order is
	// delivered.
	BinUOPRemaining *int   `json:"bin_uop_remaining,omitempty"`
	PayloadCode     string `json:"payload_code"`
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
	OldStatus protocol.Status `json:"old_status"`
	NewStatus protocol.Status `json:"new_status"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}
