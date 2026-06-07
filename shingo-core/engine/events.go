package engine

import (
	"time"

	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingocore/fleet"
	"shingocore/store/cms"
)

const (
	EventOrderReceived EventType = iota + 1
	EventOrderDispatched
	EventOrderStatusChanged
	EventOrderCompleted
	EventOrderFailed
	EventOrderSkipped
	EventOrderCancelled
	EventOrderQueued
	EventBinUpdated
	// EventLinesideBucketApplied — emitted after CoreDataService
	// successfully applies a LinesideBucketDelta. The UOP-threshold
	// monitor subscribes to this so bucket drains (which change loop
	// UOP without moving a bin) re-evaluate threshold crossings the
	// same way bin moves do.
	EventLinesideBucketApplied
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
	EventOrderFaulted
	EventOrderFaultedRecovered
	EventGraceExpired
	// EventCellTick — emitted after CoreDataService projects a production.tick
	// into cell_part_events (Phase E). SetupEngineListeners rebroadcasts it as
	// the SSE `cell-heartbeat` so the /missions Cells D section and the
	// /heartbeat kiosk pulse live without polling.
	EventCellTick
)

// --- Event payloads ---
//
// Each payload struct embeds eventbus.PayloadBase (zero-size marker) so
// it satisfies the sealed eventbus.Payload interface and can flow through
// SubscribeTyped / EmitTyped. Field layout is unchanged — PayloadBase is
// a struct{} embed.

type OrderReceivedEvent struct {
	eventbus.PayloadBase
	OrderID      int64
	EdgeUUID     string
	StationID    string
	OrderType    protocol.OrderType
	PayloadCode  string
	DeliveryNode string
}

type OrderDispatchedEvent struct {
	eventbus.PayloadBase
	OrderID       int64
	VendorOrderID string
	SourceNode    string
	DestNode      string
}

type OrderStatusChangedEvent struct {
	eventbus.PayloadBase
	OrderID       int64
	VendorOrderID string
	OldStatus     string
	NewStatus     string
	RobotID       string
	Detail        string
	Snapshot      *fleet.OrderSnapshot
}

type OrderCompletedEvent struct {
	eventbus.PayloadBase
	OrderID   int64
	EdgeUUID  string
	StationID string
}

type OrderFailedEvent struct {
	eventbus.PayloadBase
	OrderID   int64
	EdgeUUID  string
	StationID string
	ErrorCode string
	Detail    string
}

// OrderSkippedEvent signals an order reached terminal "skipped" — the work
// was never needed (e.g. complex evac order with no bin at any pickup
// node). Mirrors OrderFailedEvent fields; engine wiring serializes this
// to the protocol.OrderSkipped envelope so Edge can advance the linked
// changeover node task without surfacing a failure to the operator.
type OrderSkippedEvent struct {
	eventbus.PayloadBase
	OrderID   int64
	EdgeUUID  string
	StationID string
	ErrorCode string
	Detail    string
}

type OrderCancelledEvent struct {
	eventbus.PayloadBase
	OrderID        int64
	EdgeUUID       string
	StationID      string
	Reason         string
	PreviousStatus string // status before cancellation â€” used to skip auto-return for delivered/confirmed orders
}

type OrderQueuedEvent struct {
	eventbus.PayloadBase
	OrderID     int64
	EdgeUUID    string
	StationID   string
	PayloadCode string
}

type BinUpdatedEvent struct {
	eventbus.PayloadBase
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

// CellTickEvent carries one projected production tick to the SSE layer
// (Phase E). Station is cell_part_events.cell_id; the frontend matches it plus
// ProcessID against its cell_config to know which cell/dot to pulse.
type CellTickEvent struct {
	eventbus.PayloadBase
	Station    string
	ProcessID  int64
	StyleID    int64
	RecordedAt time.Time
}

// LinesideBucketAppliedEvent is the engine event the UOP-threshold
// monitor consumes when a bucket delta lands. PayloadCode may be
// empty for orphan / pre-upgrade-backfill rows; the monitor short-
// circuits on empty.
//
// Round-3 Obs 8: CoreNodeName replaced NodeID — the wire envelope now
// carries the cross-system identifier, and downstream consumers
// inherit the same shape.
type LinesideBucketAppliedEvent struct {
	eventbus.PayloadBase
	Station      string
	CoreNodeName string
	PayloadCode  string
	Delta        int
	Reason       protocol.LinesideBucketDeltaReason
}

type NodeUpdatedEvent struct {
	eventbus.PayloadBase
	NodeID   int64
	NodeName string
	Action   string // "created", "updated", "deleted"
}

type CorrectionAppliedEvent struct {
	eventbus.PayloadBase
	CorrectionID   int64
	CorrectionType string
	NodeID         int64
	Reason         string
	Actor          string
}

type ConnectionEvent struct {
	eventbus.PayloadBase
	Detail string
}

type RobotsUpdatedEvent struct {
	eventbus.PayloadBase
	Robots []fleet.RobotStatus
}

type CMSTransactionEvent struct {
	eventbus.PayloadBase
	Transactions []*cms.Transaction
}

// CountGroupTransitionEvent is emitted by the countgroup Runner whenever
// an advanced zone's debounced occupancy flips (or the RDS-down fail-safe
// fires). A wiring subscriber picks it up and ships it to edge via the outbox.
type CountGroupTransitionEvent struct {
	eventbus.PayloadBase
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
	eventbus.PayloadBase
	OrderID       int64
	VendorOrderID string
	BlockID       string
	Location      string
	BinTask       string
}

// BinEnteredTransitEvent fires when a bin's NodeID transitions to the
// synthetic _TRANSIT node â€” the moment the source slot is freed for
// new placements. Subscribers: the fulfillment scanner trigger (so
// queued orders re-check their dispatch eligibility against the now-
// vacant source slot) and the materials/admin UI for live transit-lane
// rendering.
type BinEnteredTransitEvent struct {
	eventbus.PayloadBase
	BinID      int64
	OrderID    int64 // the order whose pickup drove the transition
	FromNodeID int64 // the node the bin just left (now vacant)
	StepIndex  int   // position in the order's pickup sequence (0 for single-pickup)
}

// OrderFaultedEvent fires when an order enters the faulted grace-period state.
// The HMI uses this to show an amber indicator with elapsed-time-in-state so
// operators can distinguish a brief blip from an about-to-escalate fault.
type OrderFaultedEvent struct {
	eventbus.PayloadBase
	OrderID   int64
	EdgeUUID  string
	StationID string
	Reason    string
}

// OrderFaultedRecoveredEvent fires when an order transitions from faulted back
// to in_transit (fleet recovered within the grace window).
type OrderFaultedRecoveredEvent struct {
	eventbus.PayloadBase
	OrderID   int64
	EdgeUUID  string
	StationID string
	RobotID   string
}

// GraceExpiredEvent fires when the poller detects a faulted order whose
// grace period has elapsed without fleet recovery. The engine handler
// calls CancelOrder (best-effort) then Fail() for the local terminal transition.
type GraceExpiredEvent struct {
	eventbus.PayloadBase
	OrderID       int64
	VendorOrderID string
}
