package engine

import (
	"shingocore/fleet"
	"shingocore/store"
)

const (
	EventOrderReceived EventType = iota + 1
	EventOrderDispatched
	EventOrderStatusChanged
	EventOrderCompleted
	EventOrderFailed
	EventOrderCancelled
	EventPayloadChanged
	EventNodeUpdated
	EventCorrectionApplied
	EventFleetConnected
	EventFleetDisconnected
	EventMessagingConnected
	EventMessagingDisconnected
	EventDBConnected
	EventDBDisconnected
	EventRobotsUpdated
	EventCMSTransaction
)

// --- Event payloads ---

type OrderReceivedEvent struct {
	OrderID       int64
	EdgeUUID      string
	StationID     string
	OrderType     string
	BlueprintCode string
	DeliveryNode  string
}

type OrderDispatchedEvent struct {
	OrderID       int64
	VendorOrderID string
	SourceNode    string
	DestNode      string
}

type OrderStatusChangedEvent struct {
	OrderID       int64
	VendorOrderID string
	OldStatus     string
	NewStatus     string
	RobotID       string
	Detail        string
}

type OrderCompletedEvent struct {
	OrderID  int64
	EdgeUUID string
	StationID string
}

type OrderFailedEvent struct {
	OrderID  int64
	EdgeUUID string
	StationID string
	ErrorCode string
	Detail    string
}

type OrderCancelledEvent struct {
	OrderID  int64
	EdgeUUID string
	StationID string
	Reason   string
}

type PayloadChangedEvent struct {
	NodeID        int64
	NodeName      string
	Action        string // "added", "removed", "moved", "claimed", "unclaimed"
	PayloadID     int64
	BlueprintCode string
	FromNodeID    int64
	ToNodeID      int64
}

type NodeUpdatedEvent struct {
	NodeID   int64
	NodeName string
	Action   string // "created", "updated", "deleted"
}

type CorrectionAppliedEvent struct {
	CorrectionID   int64
	CorrectionType string
	NodeID         int64
	Reason         string
	Actor          string
}

type ConnectionEvent struct {
	Detail string
}

type RobotsUpdatedEvent struct {
	Robots []fleet.RobotStatus
}

type CMSTransactionEvent struct {
	Transactions []*store.CMSTransaction
}
