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

// AckOutcome is the typed outcome of a CountGroupAck. Wraps string for
// SQL/JSON-native serialization while gaining compile-time distinction
// from raw strings and other enum-shaped types in this package.
type AckOutcome string

// CountGroupAck.Outcome values.
// Use these constants instead of string literals at every call site.
const (
	AckOutcomeAcked      AckOutcome = "acked"         // PLC ladder cleared the request tag.
	AckOutcomeTimeout    AckOutcome = "ack_timeout"   // PLC did not clear within ack_dead; edge abandoned the request.
	AckOutcomeWarlinkErr AckOutcome = "warlink_error" // WarLink read or write failed.
)

// Roles for Address.Role.
const (
	RoleEdge = "edge"
	RoleCore = "core"
)

// ClaimRole is the typed role a node plays inside a process style claim:
// what kind of material flow happens at this node. Distinct from
// Address.Role (network identity, "edge"/"core") despite sharing the
// English word "role" — the two are unrelated and using the same type
// for both would be misleading.
//
// Cross-module: this value crosses Edge ↔ Core boundaries via
// ClaimSyncEntry.Role and DemandSignal.Role; the typed alias keeps
// JSON serialization byte-identical to the prior untyped string while
// giving Go callers compile-time distinction from raw strings.
type ClaimRole string

const (
	ClaimRoleConsume    ClaimRole = "consume"    // node consumes a material payload from upstream
	ClaimRoleProduce    ClaimRole = "produce"    // node produces a material payload for downstream
	ClaimRoleChangeover ClaimRole = "changeover" // node holds material that follows the line through changeover
)

// OrderType is the typed kind-of-order for fulfillment orders. Used by
// edge `orders.Order.OrderType` and the core dispatch layer; centralised
// here so both sides agree on the canonical values and so the JSON wire
// shape (raw string) stays byte-identical to the prior untyped form.
//
// Edge has all five values; core dispatch only emits Retrieve/Store/Move/
// Complex (Ingest is an edge-internal lifecycle for produce nodes).
type OrderType string

const (
	OrderTypeRetrieve OrderType = "retrieve" // pull a payload from a source to a destination
	OrderTypeStore    OrderType = "store"    // push a payload from a node to storage
	OrderTypeMove     OrderType = "move"     // generic move; no manifest semantics
	OrderTypeComplex  OrderType = "complex"  // multi-step order composed of sub-steps
	OrderTypeIngest   OrderType = "ingest"   // edge-only: produce node ingests a finished bin
)

// StationBroadcast is the wildcard station value that matches all edge instances.
const StationBroadcast = "*"

// Protocol version.
const Version = 1

// validTransitions defines the canonical state machine for order status transitions.
// Edge uses a subset (no sourcing/dispatched); Core uses the full set.
//
// IsTerminal is derived from this table: a status is terminal iff it has no
// outgoing edges (no key in this map). Adding a new non-terminal status
// requires adding a key with at least one outgoing edge here; the
// TestEveryKeyHasOutgoingEdge test enforces that invariant.
var validTransitions = map[Status][]Status{
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
func IsTerminal(status Status) bool {
	_, hasOutgoing := validTransitions[status]
	return !hasOutgoing
}

// IsValidTransition returns true if transitioning from -> to is a valid state change.
// A terminal `from` (no key in validTransitions) returns false via the lookup miss.
func IsValidTransition(from, to Status) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	return slices.Contains(allowed, to)
}

// AllValidTransitions returns a copy of the validTransitions map for test
// use. Returns a copy to prevent test mutation of the canonical table.
func AllValidTransitions() map[Status][]Status {
	out := make(map[Status][]Status, len(validTransitions))
	for from, allowed := range validTransitions {
		out[from] = append([]Status(nil), allowed...)
	}
	return out
}
