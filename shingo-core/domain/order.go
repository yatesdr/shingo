package domain

import (
	"time"

	"shingo/protocol"
)

// Order is a unit of work produced by the edge-station protocol and
// executed by the fleet. An Order tracks its source and delivery
// nodes, vendor-side identifiers once dispatched, the claim on a bin
// (for simple orders), and a parent/sequence pair for complex orders
// whose child legs are separate Order rows.
//
// StepsJSON is the serialised step list for complex orders; WaitIndex
// marks how many wait segments have been released. BinID is set for
// simple orders once the resolver picks a source bin; complex orders
// use the order_bins junction (OrderBin) to track multiple claimed
// bins, one per step.
type Order struct {
	ID              int64              `json:"id"`
	EdgeUUID        string             `json:"edge_uuid"`
	StationID       string             `json:"station_id"`
	OrderType       protocol.OrderType `json:"order_type"`
	Status          protocol.Status    `json:"status"`
	Quantity        int64              `json:"quantity"`
	SourceNode      string             `json:"source_node"`
	DeliveryNode    string             `json:"delivery_node"`
	ProcessNode     string             `json:"process_node,omitempty"`
	VendorOrderID   string             `json:"vendor_order_id"`
	VendorState     string             `json:"vendor_state"`
	RobotID         string             `json:"robot_id"`
	Priority        int                `json:"priority"`
	PayloadDesc     string             `json:"payload_desc"`
	ErrorDetail     string             `json:"error_detail"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	CompletedAt     *time.Time         `json:"completed_at,omitempty"`
	ParentOrderID   *int64             `json:"parent_order_id,omitempty"`
	Sequence        int                `json:"sequence"`
	StepsJSON       string             `json:"steps_json,omitempty"`
	BinID           *int64             `json:"bin_id,omitempty"`
	PayloadCode     string             `json:"payload_code"`
	WaitIndex       int                `json:"wait_index"`
	QueueReason     string             `json:"queue_reason,omitempty"`
	SkipAutoConfirm bool               `json:"skip_auto_confirm"`
	// SiblingOrderUUID is the edge UUID of the paired leg in a two-robot
	// swap — the supply's UUID recorded on the evac row (and vice-versa).
	// Written ATOMICALLY in the CreateOrder INSERT so a two-robot evac's
	// link to its supply can never be lost by a failed post-create link
	// step; the swapRemovalLegHeld starvation gate depends on it to avoid
	// the ALN_003 line-strand. "" for every non-swap order.
	SiblingOrderUUID string `json:"sibling_order_uuid,omitempty"`
}
