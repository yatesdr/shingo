package protocol

import "slices"

// Message type constants for the unified protocol.
const (
	// Generic data channel
	TypeData = "data"

	// Edge -> Core (published on orders topic)
	TypeOrderRequest        = "order.request"
	TypeOrderCancel         = "order.cancel"
	TypeOrderReceipt        = "order.receipt"
	TypeOrderRedirect       = "order.redirect"
	TypeOrderStorageWaybill = "order.storage_waybill"

	// Edge -> Core: complex order lifecycle
	TypeComplexOrderRequest = "order.complex_request"
	TypeOrderRelease        = "order.release"

	// Edge -> Core: origination
	TypeOrderIngest = "order.ingest"

	// Core -> Edge (published on dispatch topic)
	TypeOrderAck       = "order.ack"
	TypeOrderWaybill   = "order.waybill"
	TypeOrderUpdate    = "order.update"
	TypeOrderDelivered = "order.delivered"
	TypeOrderError     = "order.error"
	TypeOrderCancelled = "order.cancelled"
	TypeOrderStaged    = "order.staged"
)

// Data channel subject constants.
const (
	SubjectEdgeRegister     = "edge.register"
	SubjectEdgeRegistered   = "edge.registered"
	SubjectEdgeHeartbeat    = "edge.heartbeat"
	SubjectEdgeHeartbeatAck = "edge.heartbeat_ack"

	SubjectProductionReport    = "production.report"
	SubjectProductionReportAck = "production.report_ack"

	SubjectEdgeStale           = "edge.stale"
	SubjectEdgeRegisterRequest = "edge.register_request"

	SubjectNodeListRequest  = "node.list_request"
	SubjectNodeListResponse = "node.list_response"

	SubjectTagVerifyRequest  = "tag.verify_request"
	SubjectTagVerifyResponse = "tag.verify_response"

	SubjectCatalogPayloadsRequest  = "catalog.payloads_request"
	SubjectCatalogPayloadsResponse = "catalog.payloads_response"

	SubjectNodeStateRequest  = "node.state_request"
	SubjectNodeStateResponse = "node.state_response"

	SubjectOrderStatusRequest  = "order.status_request"
	SubjectOrderStatusResponse = "order.status_response"
)

// Roles for Address.Role.
const (
	RoleEdge = "edge"
	RoleCore = "core"
)

// StationBroadcast is the wildcard station value that matches all edge instances.
const StationBroadcast = "*"

// Protocol version.
const Version = 1

// Canonical order status constants shared by core and edge.
const (
	StatusPending      = "pending"
	StatusSourcing     = "sourcing"
	StatusQueued       = "queued"
	StatusSubmitted    = "submitted"
	StatusDispatched   = "dispatched"
	StatusAcknowledged = "acknowledged"
	StatusInTransit    = "in_transit"
	StatusDelivered    = "delivered"
	StatusConfirmed    = "confirmed"
	StatusStaged       = "staged"
	StatusFailed       = "failed"
	StatusCancelled    = "cancelled"
)

// IsTerminal returns true if the status is a terminal state (no further transitions).
func IsTerminal(status string) bool {
	return status == StatusConfirmed || status == StatusCancelled || status == StatusFailed
}

// validTransitions defines the canonical state machine for order status transitions.
// Edge uses a subset (no sourcing/dispatched); Core uses the full set.
var validTransitions = map[string][]string{
	StatusPending:      {StatusSourcing, StatusSubmitted, StatusCancelled, StatusFailed},
	StatusSourcing:     {StatusQueued, StatusSubmitted, StatusCancelled, StatusFailed},
	StatusSubmitted:    {StatusAcknowledged, StatusQueued, StatusCancelled, StatusFailed},
	StatusQueued:       {StatusAcknowledged, StatusInTransit, StatusCancelled, StatusFailed},
	StatusAcknowledged: {StatusDispatched, StatusInTransit, StatusCancelled, StatusFailed},
	StatusDispatched:   {StatusInTransit, StatusDelivered, StatusCancelled, StatusFailed},
	StatusInTransit:    {StatusDelivered, StatusStaged, StatusCancelled, StatusFailed},
	StatusStaged:       {StatusInTransit, StatusDelivered, StatusCancelled, StatusFailed},
	StatusDelivered:    {StatusConfirmed, StatusCancelled, StatusFailed},
}

// IsValidTransition returns true if transitioning from -> to is a valid state change.
// Terminal states cannot transition.
func IsValidTransition(from, to string) bool {
	if IsTerminal(from) {
		return false
	}
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	return slices.Contains(allowed, to)
}
