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
	TypeOrderSkipped   = "order.skipped"
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
	SubjectClaimSync    = "claim.sync"    // Edge -> Core: manual_swap claim config
	SubjectDemandSignal = "demand.signal" // Core -> Edge: kanban demand trigger

	// UOP-threshold replenishment (C-push):
	//   Core observes combined inventory (bins + buckets) per payload,
	//   compares against the configured threshold from demand_registry,
	//   and emits LoopBelowThresholdSignal on threshold crossing. Edge
	//   fires L1 retrieve_empty on receipt after countLoaderInFlightEmptyIn
	//   dedup. The legacy DemandSignal path skips opted-in (loader,
	//   payload) pairs.
	SubjectLoopBelowThreshold = "demand.loop_below_threshold" // Core -> Edge

	// Count-group light alerts (advanced-zone occupancy → PLC-driven warning light)
	SubjectCountGroupCommand = "countgroup.command" // Core -> Edge: requested light state for a zone
	SubjectCountGroupAck     = "countgroup.ack"     // Edge -> Core: PLC ack outcome for a prior command

	// Inventory delta envelopes. Edge → Core. Carry signed count
	// changes against bins and lineside buckets. Routed on subject
	// via CoreDataService's SubjectRouter — chosen over new typed
	// envelope types when the bin-as-truth refactor shipped (the
	// pre-router rationale was that new MessageHandler methods would
	// silently no-op through the InboxDedup decorator; the router
	// removed that hazard but the subject-based shape stuck because
	// it was already on the wire). Dedup is at the message level
	// via the inventory_delta_dedup table keyed on
	// (station, scope_kind, scope_key).
	SubjectBinUOPDelta         = "inventory.bin_uop_delta"
	SubjectLinesideBucketDelta = "inventory.lineside_bucket_delta"

	// BinPickedUp — Core notifies Edge when a bin is physically picked
	// up by a robot. Used by the SEND PARTIAL BACK flow: the operator
	// releases a partial bin and the cell keeps cycling; ticks during
	// the pickup window must keep attributing to
	// the released bin until the robot actually grabs it. BinPickedUp
	// is the signal that the released-bin's accumulator should flush
	// and the active claim should advance.
	SubjectBinPickedUp   = "transit.bin_picked_up"
	SubjectUOPAdjustment = "inventory.uop_adjustment" // Core -> Edge
)

// AllTypes returns every envelope Type constant in this package. Used by
// startup-coverage assertions (every Type must have a router handler
// registered at boot) and by the agreement test that pins router-and-switch
// dispatch parity during the cutover. Adding a new envelope type means
// adding both the const above and an entry here — the agreement test fails
// loudly if either side is missing.
func AllTypes() []string {
	return []string{
		TypeData,
		TypeOrderRequest,
		TypeOrderCancel,
		TypeOrderReceipt,
		TypeOrderRedirect,
		TypeOrderStorageWaybill,
		TypeComplexOrderRequest,
		TypeOrderRelease,
		TypeOrderIngest,
		TypeOrderAck,
		TypeOrderWaybill,
		TypeOrderUpdate,
		TypeOrderDelivered,
		TypeOrderError,
		TypeOrderCancelled,
		TypeOrderStaged,
		TypeOrderSkipped,
	}
}

// CoreInboundSubjects returns every Subject Core handles (envelopes
// originated by Edge: requests, lifecycle, claim sync, inventory deltas).
// Used by cmd/shingocore/main.go's boot-time SubjectRouter coverage
// assertion — adding a new Core-handled subject means adding both the
// const above and an entry here, else boot crashes loudly instead of
// dropping the first inbound envelope on the floor.
func CoreInboundSubjects() []string {
	return []string{
		SubjectEdgeRegister,
		SubjectEdgeHeartbeat,
		SubjectNodeListRequest,
		SubjectProductionReport,
		SubjectTagVerifyRequest,
		SubjectCatalogPayloadsRequest,
		SubjectNodeStateRequest,
		SubjectOrderStatusRequest,
		SubjectClaimSync,
		SubjectCountGroupAck,
		SubjectBinUOPDelta,
		SubjectLinesideBucketDelta,
	}
}

// EdgeInboundSubjects returns every Subject Edge handles (envelopes
// originated by Core: registration acks, node-list/catalog responses,
// demand signals, count-group commands, bin-picked-up notifications).
// Used by cmd/shingoedge/main.go's boot-time SubjectRouter coverage
// assertion.
//
// Core inbound subjects and Edge inbound subjects are disjoint by
// design — Subject names carry directionality, so registering a Core
// subject on Edge's router would be a wiring bug, not a no-op like the
// envelope-type case where both sides legitimately register all 17 (Edge
// no-ops the order-channel types it sends but never receives).
func EdgeInboundSubjects() []string {
	return []string{
		SubjectEdgeRegistered,
		SubjectEdgeHeartbeatAck,
		SubjectNodeListResponse,
		SubjectProductionReportAck,
		SubjectCatalogPayloadsResponse,
		SubjectOrderStatusResponse,
		SubjectTagVerifyResponse,
		SubjectEdgeRegisterRequest,
		SubjectEdgeStale,
		SubjectNodeStructureChanged,
		SubjectDemandSignal,
		SubjectLoopBelowThreshold,
		SubjectCountGroupCommand,
		SubjectBinPickedUp,
		SubjectUOPAdjustment,
	}
}

// AllSubjects returns the union of CoreInboundSubjects and
// EdgeInboundSubjects — every Subject constant that participates in
// production dispatch. Convenience for tests that walk the full set
// (e.g., a future "every subject has a sample payload in the agreement
// table" check). The boot-time coverage assertions in each composition
// root use the role-specific lists above.
func AllSubjects() []string {
	core := CoreInboundSubjects()
	edge := EdgeInboundSubjects()
	out := make([]string, 0, len(core)+len(edge))
	out = append(out, core...)
	out = append(out, edge...)
	return out
}

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
	ClaimRoleConsume ClaimRole = "consume" // node consumes a material payload from upstream
	ClaimRoleProduce ClaimRole = "produce" // node produces a material payload for downstream
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
	OrderTypeRetrieve      OrderType = "retrieve"       // pull a loaded bin matching a payload to a destination
	OrderTypeRetrieveEmpty OrderType = "retrieve_empty" // pull an empty bin compatible with a payload to a destination
	OrderTypeStore         OrderType = "store"          // push a payload from a node to storage
	OrderTypeMove          OrderType = "move"           // generic move; no manifest semantics
	OrderTypeComplex       OrderType = "complex"        // multi-step order composed of sub-steps
	OrderTypeIngest        OrderType = "ingest"         // edge-only: produce node ingests a finished bin
	// OrderTypeReshuffleRestore is a Core-internal housekeeping order
	// that wraps the post-pickup restock compound for the complex-order
	// buried-bin reshuffle "restore blockers" toggle. Never created by
	// edge; not dispatched to edge; filtered out of the admin orders
	// list. The synthetic-parent type exists so the restock compound
	// has a parent row to satisfy AdvanceCompoundOrder, since the
	// compound machinery keys off ParentOrderID != nil.
	OrderTypeReshuffleRestore OrderType = "reshuffle_restore"
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
	StatusPending: {StatusSourcing, StatusSubmitted, StatusQueued, StatusReshuffling, StatusCancelled, StatusFailed, StatusSkipped},

	// Sourcing → Dispatched is the immediate write after fleet.CreateOrder
	// when inventory is available at planning time and the order skips the
	// Queued state. Sourcing → Reshuffling is the planning-time pivot when
	// the resolver detects a buried bin and creates a compound parent.
	StatusSourcing: {StatusQueued, StatusSubmitted, StatusDispatched, StatusReshuffling, StatusCancelled, StatusFailed, StatusSkipped},

	StatusSubmitted: {StatusAcknowledged, StatusQueued, StatusCancelled, StatusFailed, StatusSkipped},

	// Queued → Dispatched is the immediate write after fleet CreateOrder
	// returns; Acknowledged is reported asynchronously by the vendor later.
	// Queued → Sourcing supports the scanner's re-resolve path when an
	// inflight bin claim becomes invalid.
	// Queued → Skipped is fired by DispatchPreparedComplex when claimComplexBins
	// finds zero bins at every pickup node — the work was never needed.
	// Queued → Reshuffling supports the complex-order buried-source path:
	// complex intake creates the parent at Queued, then pivots to
	// Reshuffling when the resolver returns *BuriedError. (Simple
	// retrieves take Pending → Reshuffling instead; the parent is still
	// in pending when planning sees the burial.)
	StatusQueued: {StatusAcknowledged, StatusDispatched, StatusInTransit, StatusSourcing, StatusReshuffling, StatusCancelled, StatusFailed, StatusSkipped},

	// Acknowledged|Dispatched → Sourcing supports PrepareRedirect: the order
	// is re-resolved against a new delivery node after the vendor leg is
	// cancelled.
	StatusAcknowledged: {StatusDispatched, StatusInTransit, StatusSourcing, StatusCancelled, StatusFaulted, StatusFailed},
	StatusDispatched:   {StatusInTransit, StatusDelivered, StatusSourcing, StatusCancelled, StatusFaulted, StatusFailed},

	StatusInTransit: {StatusDelivered, StatusStaged, StatusCancelled, StatusFaulted, StatusFailed},
	StatusStaged:    {StatusInTransit, StatusDelivered, StatusCancelled, StatusFaulted, StatusFailed},

	// Faulted is the non-terminal grace-period status entered when the fleet
	// reports a transient failure (RDS FAILED). Orders stay faulted until
	// either the fleet recovers (faulted→in_transit), the operator manually
	// finishes (faulted→delivered), or the grace period expires and Core
	// gives up (faulted→failed) or the operator cancels (faulted→cancelled).
	StatusFaulted:   {StatusInTransit, StatusDelivered, StatusFailed, StatusCancelled},
	StatusDelivered: {StatusConfirmed, StatusCancelled, StatusFailed},
	// Reshuffling → Queued is the complex-order resume edge: after a
	// compound completes successfully, the complex parent transitions
	// back to Queued so the fulfillment scanner picks it up and
	// re-resolves its original pickup step against the now-accessible
	// slot. Simple-retrieve compounds still terminate at Confirmed —
	// see dispatch/compound.go AdvanceCompoundOrder routing.
	StatusReshuffling: {StatusConfirmed, StatusQueued, StatusCancelled, StatusFailed},
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
