package engine

import (
	"time"

	"shingocore/fleet"
	"shingocore/store/cms"
)


const (
	EventOrderReceived EventType = iota + 1
	EventOrderDispatched
	EventOrderStatusChanged
	EventOrderCompleted
	EventOrderFailed
	EventOrderCancelled
	EventOrderQueued
	EventBinUpdated
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
	EventCountGroupTransition
)

// --- Event payloads ---

type OrderReceivedEvent struct {
	OrderID      int64
	EdgeUUID     string
	StationID    string
	OrderType    string
	PayloadCode  string
	DeliveryNode string
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
	Snapshot      *fleet.OrderSnapshot
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
	OrderID        int64
	EdgeUUID       string
	StationID      string
	Reason         string
	PreviousStatus string // status before cancellation — used to skip auto-return for delivered/confirmed orders
}

type OrderQueuedEvent struct {
	OrderID     int64
	EdgeUUID    string
	StationID   string
	PayloadCode string
}

type BinUpdatedEvent struct {
	NodeID      int64
	NodeName    string
	Action      string // "added", "removed", "moved", "claimed", "unclaimed", "locked", "unlocked", "loaded", "cleared", "counted", "status_changed"
	BinID       int64
	PayloadCode string
	FromNodeID  int64
	ToNodeID    int64
	Actor       string
	Detail      string
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
	Transactions []*cms.Transaction
}

// CountGroupTransitionEvent is emitted by the countgroup Runner whenever
// an advanced zone's debounced occupancy flips (or the RDS-down fail-safe
// fires). A wiring subscriber picks it up and ships it to edge via the outbox.
type CountGroupTransitionEvent struct {
	Group             string
	Desired           string // "on" | "off"
	Robots            []string
	FailSafeTriggered bool
	Timestamp         time.Time
}
