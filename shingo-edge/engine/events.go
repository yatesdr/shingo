package engine

import (
	"shingo/protocol"
	"shingo/protocol/eventbus"
)

// EventType identifies the kind of event emitted by the Engine.
type EventType int

const (
	// Counter events
	EventCounterRead EventType = iota + 1
	EventCounterDelta
	EventCounterAnomaly
	EventCounterReadError

	// Order events
	EventOrderCreated
	EventOrderStatusChanged
	EventOrderCompleted
	EventOrderFailed

	// PLC events
	EventPLCConnected
	EventPLCDisconnected
	EventPLCHealthAlert
	EventPLCHealthRecover

	// WarLink events
	EventWarLinkConnected
	EventWarLinkDisconnected

	// Core node sync events
	EventCoreNodesUpdated
)

// Event is the envelope emitted by the Engine's EventBus.
type Event = eventbus.Event[EventType]

// CounterReadEvent is emitted on every PLC poll.
type CounterReadEvent struct {
	ReportingPointID int64  `json:"reporting_point_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	Value            int64  `json:"value"`
}

// CounterDeltaEvent is emitted when production count increases.
type CounterDeltaEvent struct {
	ReportingPointID int64  `json:"reporting_point_id"`
	ProcessID        int64  `json:"process_id"`
	StyleID          int64  `json:"style_id"`
	Delta            int64  `json:"delta"`
	NewCount         int64  `json:"new_count"`
	Anomaly          string `json:"anomaly"` // "reset" if from a PLC counter reset, "" for normal
}

// CounterAnomalyEvent is emitted for counter resets or jumps.
type CounterAnomalyEvent struct {
	ReportingPointID int64  `json:"reporting_point_id"`
	SnapshotID       int64  `json:"snapshot_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	OldValue         int64  `json:"old_value"`
	NewValue         int64  `json:"new_value"`
	AnomalyType      string `json:"anomaly_type"` // "reset" or "jump"
}

// OrderCreatedEvent is emitted when a new order is placed.
type OrderCreatedEvent struct {
	OrderID       int64  `json:"order_id"`
	OrderUUID     string `json:"order_uuid"`
	OrderType     string `json:"order_type"`
	ProcessNodeID *int64 `json:"process_node_id,omitempty"`
}

// OrderStatusChangedEvent is emitted on order state transitions.
type OrderStatusChangedEvent struct {
	OrderID       int64  `json:"order_id"`
	OrderUUID     string `json:"order_uuid"`
	OrderType     string `json:"order_type"`
	OldStatus     string `json:"old_status"`
	NewStatus     string `json:"new_status"`
	ETA           string `json:"eta"`
	ProcessNodeID *int64 `json:"process_node_id,omitempty"`
}

// OrderCompletedEvent is emitted when an order reaches terminal state.
type OrderCompletedEvent struct {
	OrderID       int64  `json:"order_id"`
	OrderUUID     string `json:"order_uuid"`
	OrderType     string `json:"order_type"`
	ProcessNodeID *int64 `json:"process_node_id,omitempty"`
}

// PLCEvent is emitted for PLC connection state changes.
type PLCEvent struct {
	PLCName string `json:"plc_name"`
	Error   string `json:"error,omitempty"`
}

// PLCHealthAlertEvent is emitted when a PLC goes offline.
type PLCHealthAlertEvent struct {
	PLCName string `json:"plc_name"`
	Error   string `json:"error,omitempty"`
}

// PLCHealthRecoverEvent is emitted when a PLC comes back online.
type PLCHealthRecoverEvent struct {
	PLCName string `json:"plc_name"`
}

// WarLinkEvent is emitted when the WarLink connection state changes.
type WarLinkEvent struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

// CoreNodesUpdatedEvent is emitted when the core node list is received.
type CoreNodesUpdatedEvent struct {
	Nodes []protocol.NodeInfo `json:"nodes"`
}

// CounterReadErrorEvent is emitted when a tag read fails.
type CounterReadErrorEvent struct {
	ReportingPointID int64  `json:"reporting_point_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	Error            string `json:"error"`
}

// OrderFailedEvent is emitted when an order transitions to failed state.
type OrderFailedEvent struct {
	OrderID   int64  `json:"order_id"`
	OrderUUID string `json:"order_uuid"`
	OrderType string `json:"order_type"`
	Reason    string `json:"reason"`
}
