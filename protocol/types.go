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

	SubjectNodeStructureChanged = "node.structure_changed"

	// Kanban demand wiring (Phase 2)
	SubjectClaimSync   = "claim.sync"    // Edge -> Core: manual_swap claim config
	SubjectDemandSignal = "demand.signal" // Core -> Edge: kanban demand trigger

	// Count-group light alerts (advanced-zone occupancy → PLC-driven warning light)
	SubjectCountGroupCommand = "countgroup.command" // Core -> Edge: requested light state for a zone
	SubjectCountGroupAck     = "countgroup.ack"     // Edge -> Core: PLC ack outcome for a prior command
)

// CountGroupAck.Outcome values.
// Use these constants instead of string literals at every call site.
const (
	AckOutcomeAcked      = "acked"        // PLC ladder cleared the request tag.
	AckOutcomeTimeout    = "ack_timeout"  // PLC did not clear within ack_dead; edge abandoned the request.
	AckOutcomeWarlinkErr = "warlink_error" // WarLink read or write failed.
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
	StatusReshuffling  = "reshuffling"
)

// validTransitions defines the canonical state machine for order status transitions.
// Edge uses a subset (no sourcing/dispatched); Core uses the full set.
//
// IsTerminal is derived from this table: a status is terminal iff it has no
// outgoing edges (no key in this map). Adding a new non-terminal status
// requires adding a key with at least one outgoing edge here; the
// TestEveryKeyHasOutgoingEdge test enforces that invariant.
var validTransitions = map[string][]string{
	// Pending → Queued is a fast-path used by fulfillment/scanner.go when
	// the bin is already known and resolution can be skipped.
	StatusPending: {StatusSourcing, StatusSubmitted, StatusQueued, StatusReshuffling, StatusCancelled, StatusFailed},

	// Sourcing → Dispatched is the immediate write after fleet.CreateOrder
	// when inventory is available at planning time and the order skips the
	// Queued state. Sourcing → Reshuffling is the planning-time pivot when
	// the resolver detects a buried bin and creates a compound parent.
	StatusSourcing: {StatusQueued, StatusSubmitted, StatusDispatched, StatusReshuffling, StatusCancelled, StatusFailed},

	StatusSubmitted: {StatusAcknowledged, StatusQueued, StatusCancelled, StatusFailed},

	// Queued → Dispatched is the immediate write after fleet CreateOrder
	// returns; Acknowledged is reported asynchronously by the vendor later.
	// Queued → Sourcing supports the scanner's re-resolve path when an
	// inflight bin claim becomes invalid.
	StatusQueued: {StatusAcknowledged, StatusDispatched, StatusInTransit, StatusSourcing, StatusCancelled, StatusFailed},

	// Acknowledged|Dispatched → Sourcing supports PrepareRedirect: the order
	// is re-resolved against a new delivery node after the vendor leg is
	// cancelled.
	StatusAcknowledged: {StatusDispatched, StatusInTransit, StatusSourcing, StatusCancelled, StatusFailed},
	StatusDispatched:   {StatusInTransit, StatusDelivered, StatusSourcing, StatusCancelled, StatusFailed},

	StatusInTransit:   {StatusDelivered, StatusStaged, StatusCancelled, StatusFailed},
	StatusStaged:      {StatusInTransit, StatusDelivered, StatusCancelled, StatusFailed},
	StatusDelivered:   {StatusConfirmed, StatusCancelled, StatusFailed},
	StatusReshuffling: {StatusConfirmed, StatusCancelled, StatusFailed},
}

// IsTerminal returns true if the status has no outgoing transitions in
// validTransitions (i.e. it is not a key in the map). Single source of
// truth: adding a non-terminal status to the table no longer requires
// updating this function.
func IsTerminal(status string) bool {
	_, hasOutgoing := validTransitions[status]
	return !hasOutgoing
}

// IsValidTransition returns true if transitioning from -> to is a valid state change.
// A terminal `from` (no key in validTransitions) returns false via the lookup miss.
func IsValidTransition(from, to string) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	return slices.Contains(allowed, to)
}

// AllStatuses returns every status defined in this module, used by
// table-driven tests that exhaustively cover the (from, to) matrix.
func AllStatuses() []string {
	return []string{
		StatusPending, StatusSourcing, StatusQueued, StatusSubmitted,
		StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged,
		StatusDelivered, StatusConfirmed, StatusFailed, StatusCancelled,
		StatusReshuffling,
	}
}

// AllValidTransitions returns a copy of the validTransitions map for test
// use. Returns a copy to prevent test mutation of the canonical table.
func AllValidTransitions() map[string][]string {
	out := make(map[string][]string, len(validTransitions))
	for from, allowed := range validTransitions {
		out[from] = append([]string(nil), allowed...)
	}
	return out
}
