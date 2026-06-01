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
	EventOrderFaulted

	// EventOrderDelivered fires the moment an order transitions to
	// StatusDelivered — i.e., the bin physically arrived at its
	// destination node. The runtime UOP cache for the destination slot
	// (when DeliveryNode == process_node.CoreNodeName) flips to the
	// delivered bin's authoritative count via handleNodeOrderDelivered.
	// Distinct from EventOrderCompleted, which fires on terminal status
	// (Confirmed/Failed/Cancelled) and is reserved for operator-semantic
	// side-effects (state machine, side-cycle dispatch).
	EventOrderDelivered

	// EventProducedReport fires once per produce-node tick, carrying the
	// node's resolved payload code. The production reporter subscribes to
	// this (rather than the raw EventCounterDelta) so it keys finished-good
	// counts by the catalog part code (cat_id) Core matches demands on,
	// instead of the style name. See ProducedReportEvent.
	EventProducedReport
)

// Event is the envelope emitted by the Engine's EventBus.
type Event = eventbus.Event[EventType]

// --- Event payloads ---
//
// Each payload struct embeds eventbus.PayloadBase (zero-size marker) so
// it satisfies the sealed eventbus.Payload interface and can flow through
// SubscribeTyped / EmitTyped. Field layout is unchanged.

// CounterReadEvent is emitted on every PLC poll.
type CounterReadEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64  `json:"reporting_point_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	Value            int64  `json:"value"`
}

// CounterDeltaEvent is emitted when production count increases.
type CounterDeltaEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64  `json:"reporting_point_id"`
	ProcessID        int64  `json:"process_id"`
	StyleID          int64  `json:"style_id"`
	Delta            int64  `json:"delta"`
	NewCount         int64  `json:"new_count"`
	Anomaly          string `json:"anomaly"` // "reset" if from a PLC counter reset, "" for normal
}

// ProducedReportEvent is emitted once per produce-node tick. PayloadCode is
// the produce node's active-claim payload — the catalog part code (cat_id) —
// resolved at the tick site where the node, and therefore the part, is
// unambiguous even for multi-part styles. The production reporter keys
// counts by this instead of the style name so they match demands.cat_id on
// Core. Mirrors the per-produce-node inventory delta emitted alongside it.
type ProducedReportEvent struct {
	eventbus.PayloadBase
	PayloadCode string `json:"payload_code"`
	Delta       int64  `json:"delta"`
}

// CounterAnomalyEvent is emitted for counter resets or jumps.
type CounterAnomalyEvent struct {
	eventbus.PayloadBase
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
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
}

// OrderStatusChangedEvent is emitted on order state transitions.
type OrderStatusChangedEvent struct {
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	OldStatus     string             `json:"old_status"`
	NewStatus     string             `json:"new_status"`
	ETA           string             `json:"eta"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
}

// OrderCompletedEvent is emitted when an order reaches terminal state.
type OrderCompletedEvent struct {
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
}

// PLCEvent is emitted for PLC connection state changes.
type PLCEvent struct {
	eventbus.PayloadBase
	PLCName string `json:"plc_name"`
	Error   string `json:"error,omitempty"`
}

// PLCHealthAlertEvent is emitted when a PLC goes offline.
type PLCHealthAlertEvent struct {
	eventbus.PayloadBase
	PLCName string `json:"plc_name"`
	Error   string `json:"error,omitempty"`
}

// PLCHealthRecoverEvent is emitted when a PLC comes back online.
type PLCHealthRecoverEvent struct {
	eventbus.PayloadBase
	PLCName string `json:"plc_name"`
}

// WarLinkEvent is emitted when the WarLink connection state changes.
type WarLinkEvent struct {
	eventbus.PayloadBase
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

// CoreNodesUpdatedEvent is emitted when the core node list is received.
type CoreNodesUpdatedEvent struct {
	eventbus.PayloadBase
	Nodes []protocol.NodeInfo `json:"nodes"`
}

// CounterReadErrorEvent is emitted when a tag read fails.
type CounterReadErrorEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64  `json:"reporting_point_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	Error            string `json:"error"`
}

// OrderFailedEvent is emitted when an order transitions to failed state.
type OrderFailedEvent struct {
	eventbus.PayloadBase
	OrderID   int64              `json:"order_id"`
	OrderUUID string             `json:"order_uuid"`
	OrderType protocol.OrderType `json:"order_type"`
	Reason    string             `json:"reason"`
}

// OrderFaultedEvent is emitted when an order transitions to faulted state.
// The HMI shows an amber indicator with elapsed-time-in-state so operators
// can distinguish a brief blip from an about-to-escalate fault.
type OrderFaultedEvent struct {
	eventbus.PayloadBase
	OrderID   int64  `json:"order_id"`
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

// OrderDeliveredEvent is emitted when an order transitions to
// StatusDelivered. Carries the BinID Core resolved at delivery time so
// the delivered handler can look up the bin's authoritative
// uop_remaining and bind the slot's runtime cache to it. ProcessNodeID
// is the dispatch-time process node hint; the delivered handler still
// gates on order.DeliveryNode == process_node.CoreNodeName because
// removal-shaped orders (e.g., Order B in two-robot consume) attach to
// the process node for tracking but deliver to the supermarket.
type OrderDeliveredEvent struct {
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
	BinID         *int64             `json:"bin_id,omitempty"`
}
