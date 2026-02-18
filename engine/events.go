package engine

const (
	EventOrderReceived EventType = iota + 1
	EventOrderDispatched
	EventOrderStatusChanged
	EventOrderCompleted
	EventOrderFailed
	EventOrderCancelled
	EventInventoryChanged
	EventNodeUpdated
	EventCorrectionApplied
	EventRDSConnected
	EventRDSDisconnected
	EventMessagingConnected
	EventMessagingDisconnected
)

// --- Event payloads ---

type OrderReceivedEvent struct {
	OrderID      int64
	WardropUUID  string
	ClientID     string
	OrderType    string
	MaterialCode string
	DeliveryNode string
}

type OrderDispatchedEvent struct {
	OrderID    int64
	RDSOrderID string
	SourceNode string
	DestNode   string
}

type OrderStatusChangedEvent struct {
	OrderID    int64
	RDSOrderID string
	OldStatus  string
	NewStatus  string
	RobotID    string
	Detail     string
}

type OrderCompletedEvent struct {
	OrderID     int64
	WardropUUID string
	ClientID    string
}

type OrderFailedEvent struct {
	OrderID     int64
	WardropUUID string
	ClientID    string
	ErrorCode   string
	Detail      string
}

type OrderCancelledEvent struct {
	OrderID     int64
	WardropUUID string
	ClientID    string
	Reason      string
}

type InventoryChangedEvent struct {
	NodeID       int64
	NodeName     string
	Action       string // "added", "removed", "moved", "adjusted"
	MaterialCode string
	Quantity     float64
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
