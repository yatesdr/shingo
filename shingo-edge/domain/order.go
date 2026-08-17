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
	ID             int64              `json:"id"`
	UUID           string             `json:"uuid"`
	OrderType      protocol.OrderType `json:"order_type"`
	Status         protocol.Status    `json:"status"`
	ProcessNodeID  *int64             `json:"process_node_id,omitempty"`
	RetrieveEmpty  bool               `json:"retrieve_empty"`
	Quantity       int64              `json:"quantity"`
	DeliveryNode   string             `json:"delivery_node"`
	StagingNode    string             `json:"staging_node"`
	SourceNode     string             `json:"source_node"`
	LoadType       string             `json:"load_type"`
	WaybillID      *string            `json:"waybill_id"`
	ExternalRef    *string            `json:"external_ref"`
	FinalCount     *int64             `json:"final_count"`
	CountConfirmed bool               `json:"count_confirmed"`
	ETA            *string            `json:"eta"`
	AutoConfirm    bool               `json:"auto_confirm"`
	StagedExpireAt *time.Time         `json:"staged_expire_at,omitempty"`
	// BinID is Core's ID for the bin associated with this order,
	// snapshot from OrderDelivered. PLC tick attribution at
	// consume/produce time looks up runtime.ActiveOrderID, reads its
	// BinID, and emits BinUOPDelta against that bin. Nil for multi-bin
	// orders; older Core builds leave it nil and Edge skips bin delta
	// emission.
	BinID       *int64 `json:"bin_id,omitempty"`
	PayloadCode string `json:"payload_code"`
	// SiblingOrderID is the id of the paired order in a two-robot swap
	// (supply ↔ evac). Durable linkage so the supply guard and the
	// release gate don't depend on volatile runtime slot pointers,
	// which can be nulled by bin-pickup events before release fires.
	// Nil for non-paired orders (single-robot, simple, manual_swap).
	SiblingOrderID *int64 `json:"sibling_order_id,omitempty"`
	// QueueReason holds Core's last blocking signal for this order
	// (mirrored from Core's orders.queue_reason via OrderUpdate push).
	// HMI renders it as "IN QUEUE: <reason>" so operators can see WHY a
	// robot isn't coming.
	//
	// Non-empty only while the status is acquiring (queued|sourcing) —
	// enforced on arrival in messaging/edge_handler.HandleOrderUpdate, which
	// clears it on any non-acquiring push. This comment claimed that
	// invariant from the start; nothing held it up until 2026-08-03, and the
	// field was in practice write-once. Springfield ALN_001 displayed a
	// 2½-hour-old reason during a later changeover.
	QueueReason string `json:"queue_reason"`
	// QueueCode is the structured category behind QueueReason (mirrored from
	// Core's orders.queue_code). One of protocol.QueueCode. Edge persists it for
	// future branching (e.g. special fleet-unavailable handling) without a schema
	// change; display keeps rendering the sentence today. Empty on non-queued
	// orders. Cause never leaves Core.
	QueueCode string `json:"queue_code"`
	// AuthoredBy says who decided this order should exist: "edge" (this Edge
	// created it and sent it up) or "core" (Core created it and pushed the row
	// down as a projection). Every row that predates the column is "edge", which
	// is true — they were all created here.
	//
	// NOTHING BRANCHES ON IT, deliberately. It labels the board and it is what
	// the projection tests assert against. Keeping it inert means turning the
	// label off is a rendering change, not a behaviour change.
	AuthoredBy string `json:"authored_by"`
	// LaneHeld reports that this order is parked on a wait CORE owns — a lane
	// gate — rather than on one the station owns. Derived, never stored: an order
	// is lane-held exactly when it is `staged` and carries no Edge-authored step
	// plan.
	//
	// That derivation is exact rather than approximate. A station wait exists only
	// inside a plan this Edge wrote, so a plan-less order has no wait of its own;
	// and a plan-less order's fleet waybill is [pickup, dropoff] with no Wait
	// block, so the fleet never reports WAITING for it and it never reaches
	// `staged` by any other route. The one thing that puts a plan-less order at
	// `staged` is Core parking it at a lane's gate point.
	//
	// It exists to remove a CONTROL, not information: the tile still shows the
	// order and its status, and only the RELEASE button goes. Core refuses such a
	// release anyway (dispatch.HandleOrderRelease), so a button that survived here
	// would be one whose only correct outcome is an error.
	//
	// Unlike AuthoredBy — which is deliberately inert — this one BRANCHES, so it
	// is computed in the query beside the row it describes rather than being
	// re-derived by each caller.
	LaneHeld  bool      `json:"lane_held"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	ID        int64           `json:"id"`
	OrderID   int64           `json:"order_id"`
	OldStatus protocol.Status `json:"old_status"`
	NewStatus protocol.Status `json:"new_status"`
	Detail    string          `json:"detail"`
	CreatedAt time.Time       `json:"created_at"`
}
