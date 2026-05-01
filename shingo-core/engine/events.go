package engine

import (
	"time"

	"shingo/protocol"
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
	EventBlockCompleted
	EventBinEnteredTransit
)

// --- Event payloads ---

type OrderReceivedEvent struct {
	OrderID      int64
	EdgeUUID     string
	StationID    string
	OrderType    protocol.OrderType
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

// BlockCompletedEvent fires when a single block within a vendor order
// transitions to FINISHED while the order is still mid-flight. Phase 2
// of the bin-transit-state project: pickup blocks (BinTask=Load /
// retrieve / "pickup") drive the bin onto the synthetic _TRANSIT node
// so the source slot is freed immediately. Other block types (waits,
// drops) are observable but not currently acted on at this layer.
//
// Location is the vendor's physical location string (e.g., the dot-name
// of the pickup node). BinTask carries the vendor's binTask field
// ("Load", "Unload", "Wait", or empty for navigation-only blocks).
type BlockCompletedEvent struct {
	OrderID       int64
	VendorOrderID string
	BlockID       string
	Location      string
	BinTask       string
}

// BinEnteredTransitEvent fires when a bin's NodeID transitions to the
// synthetic _TRANSIT node — the moment the source slot is freed for
// new placements. Subscribers: the fulfillment scanner trigger (so
// queued orders re-check their dispatch eligibility against the now-
// vacant source slot) and the materials/admin UI for live transit-lane
// rendering.
type BinEnteredTransitEvent struct {
	BinID      int64
	OrderID    int64  // the order whose pickup drove the transition
	FromNodeID int64  // the node the bin just left (now vacant)
	StepIndex  int    // position in the order's pickup sequence (0 for single-pickup)
}
