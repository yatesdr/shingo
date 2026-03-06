package engine

import (
	"time"

	"shingo/protocol"
)

// EventType identifies the kind of event emitted by the Engine.
type EventType int

const (
	// Counter events
	EventCounterRead EventType = iota + 1
	EventCounterDelta
	EventCounterAnomaly
	EventCounterReadError

	// Payload events
	EventPayloadUpdated
	EventPayloadReorder
	EventPayloadEmpty

	// Order events
	EventOrderCreated
	EventOrderStatusChanged
	EventOrderCompleted
	EventOrderFailed

	// Changeover events
	EventChangeoverStarted
	EventChangeoverStateChanged
	EventChangeoverCompleted
	EventChangeoverCancelled

	// PLC events
	EventPLCConnected
	EventPLCDisconnected
	EventPLCHealthAlert
	EventPLCHealthRecover

	// WarLink events
	EventWarLinkConnected
	EventWarLinkDisconnected

	// Produce payload events
	EventPayloadNeedsEmptyBin

	// Core node sync events
	EventCoreNodesUpdated
)

// Event is the envelope emitted by the Engine's EventBus.
type Event struct {
	Type      EventType
	Timestamp time.Time
	Payload   interface{}
}

// CounterReadEvent is emitted on every PLC poll.
type CounterReadEvent struct {
	ReportingPointID int64
	PLCName          string
	TagName          string
	Value            int64
}

// CounterDeltaEvent is emitted when production count increases.
type CounterDeltaEvent struct {
	ReportingPointID int64
	LineID           int64
	JobStyleID       int64
	Delta            int64
	NewCount         int64
	Anomaly          string // "reset" if from a PLC counter reset, "" for normal
}

// CounterAnomalyEvent is emitted for counter resets or jumps.
type CounterAnomalyEvent struct {
	ReportingPointID int64
	SnapshotID       int64
	PLCName          string
	TagName          string
	OldValue         int64
	NewValue         int64
	AnomalyType      string // "reset" or "jump"
}

// PayloadUpdatedEvent is emitted when payload remaining changes.
type PayloadUpdatedEvent struct {
	PayloadID    int64  `json:"payload_id"`
	LineID       int64  `json:"line_id"`
	JobStyleID   int64  `json:"job_style_id"`
	Location     string `json:"location"`
	OldRemaining int    `json:"old_remaining"`
	NewRemaining int    `json:"new_remaining"`
	Status       string `json:"status"`
}

// PayloadReorderEvent is emitted when payload crosses reorder point.
type PayloadReorderEvent struct {
	PayloadID     int64  `json:"payload_id"`
	LineID        int64  `json:"line_id"`
	JobStyleID    int64  `json:"job_style_id"`
	Location      string `json:"location"`
	StagingNode   string `json:"staging_node"`
	Description   string `json:"description"`
	BlueprintCode string `json:"blueprint_code"`
	Remaining     int    `json:"remaining"`
	ReorderPoint  int    `json:"reorder_point"`
	ReorderQty    int    `json:"reorder_qty"`
	RetrieveEmpty bool   `json:"retrieve_empty"`
}

// OrderCreatedEvent is emitted when a new order is placed.
type OrderCreatedEvent struct {
	OrderID   int64
	OrderUUID string
	OrderType string
}

// OrderStatusChangedEvent is emitted on order state transitions.
type OrderStatusChangedEvent struct {
	OrderID   int64
	OrderUUID string
	OrderType string
	OldStatus string
	NewStatus string
	ETA       string
}

// OrderCompletedEvent is emitted when an order reaches terminal state.
type OrderCompletedEvent struct {
	OrderID   int64
	OrderUUID string
	OrderType string
}

// ChangeoverStartedEvent is emitted when a changeover begins.
type ChangeoverStartedEvent struct {
	LineID       int64  `json:"line_id"`
	FromJobStyle string `json:"from_job_style"`
	ToJobStyle   string `json:"to_job_style"`
}

// ChangeoverStateChangedEvent is emitted on changeover state transitions.
type ChangeoverStateChangedEvent struct {
	LineID       int64  `json:"line_id"`
	FromJobStyle string `json:"from_job_style"`
	ToJobStyle   string `json:"to_job_style"`
	OldState     string `json:"old_state"`
	NewState     string `json:"new_state"`
}

// ChangeoverCompletedEvent is emitted when a changeover finishes.
type ChangeoverCompletedEvent struct {
	LineID       int64  `json:"line_id"`
	FromJobStyle string `json:"from_job_style"`
	ToJobStyle   string `json:"to_job_style"`
}

// PLCEvent is emitted for PLC connection state changes.
type PLCEvent struct {
	PLCName string
	Error   string
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

// PayloadEmptyEvent is emitted when a payload's remaining count hits zero.
type PayloadEmptyEvent struct {
	PayloadID  int64  `json:"payload_id"`
	LineID     int64  `json:"line_id"`
	JobStyleID int64  `json:"job_style_id"`
	Location   string `json:"location"`
}

// PayloadNeedsEmptyBinEvent is emitted when a produce payload needs an empty bin delivered.
type PayloadNeedsEmptyBinEvent struct {
	PayloadID     int64  `json:"payload_id"`
	LineID        int64  `json:"line_id"`
	JobStyleID    int64  `json:"job_style_id"`
	Location      string `json:"location"`
	StagingNode   string `json:"staging_node"`
	BlueprintCode string `json:"blueprint_code"`
}

// OrderFailedEvent is emitted when an order transitions to failed state.
type OrderFailedEvent struct {
	OrderID   int64  `json:"order_id"`
	OrderUUID string `json:"order_uuid"`
	OrderType string `json:"order_type"`
	Reason    string `json:"reason"`
}

// ChangeoverCancelledEvent is emitted when a changeover is cancelled (distinct from completion).
type ChangeoverCancelledEvent struct {
	LineID       int64  `json:"line_id"`
	FromJobStyle string `json:"from_job_style"`
	ToJobStyle   string `json:"to_job_style"`
	Operator     string `json:"operator"`
}
