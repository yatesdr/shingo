package orders

import "shingo/protocol"

// Order types — aliased to the canonical typed constants in protocol so
// edge and core agree on the wire shape and Go callers get compile-time
// distinction from raw strings.
const (
	TypeRetrieve = protocol.OrderTypeRetrieve
	TypeStore    = protocol.OrderTypeStore
	TypeMove     = protocol.OrderTypeMove
	TypeComplex  = protocol.OrderTypeComplex
)

// Order statuses aliased from protocol.
//
// Edge mirrors Core's full status vocabulary: sourcing/dispatched/faulted are
// stored on the Edge row when Core pushes them via order.update or a boot
// snapshot, so the operator sees the truth of whichever machine owns the order
// at that moment. See orders.ApplyCoreStatus for the mapping shared by the
// live-push and snapshot paths.
const (
	StatusPending      = protocol.StatusPending
	StatusSourcing     = protocol.StatusSourcing
	StatusQueued       = protocol.StatusQueued
	StatusSubmitted    = protocol.StatusSubmitted
	StatusDispatched   = protocol.StatusDispatched
	StatusAcknowledged = protocol.StatusAcknowledged
	StatusInTransit    = protocol.StatusInTransit
	StatusStaged       = protocol.StatusStaged
	StatusDelivered    = protocol.StatusDelivered
	StatusConfirmed    = protocol.StatusConfirmed
	StatusCancelled    = protocol.StatusCancelled
	StatusFailed       = protocol.StatusFailed
	StatusSkipped      = protocol.StatusSkipped
	StatusReshuffling  = protocol.StatusReshuffling
	StatusFaulted      = protocol.StatusFaulted
)

// Dispatch reply types — used by HandleDispatchReply and edge_handler.
const (
	ReplyAck       = "ack"
	ReplyWaybill   = "waybill"
	ReplyUpdate    = "update"
	ReplyDelivered = "delivered"
	ReplyError     = "error"
	ReplySkipped   = "skipped"
	ReplyStaged    = "staged"
	ReplyCancelled = "cancelled"
	ReplyQueued    = "queued"
)

// IsValidTransition delegates to the canonical state machine in protocol.
func IsValidTransition(from, to protocol.Status) bool {
	return protocol.IsValidTransition(from, to)
}

// IsTerminal delegates to the canonical definition in protocol.
func IsTerminal(status protocol.Status) bool {
	return protocol.IsTerminal(status)
}

// ReleasableAtCore reports whether Core will ACCEPT an OrderRelease for an
// order in this status. It mirrors Core's precondition verbatim — see
// shingo-core/dispatch/complex_release.go, which rejects anything that is
// neither staged nor in_transit with an "invalid_state" error (in_transit is
// accepted for duplicate fan-out from the consolidated two-robot release and
// for multi-wait re-release).
//
// Why callers need this: Manager.ReleaseOrderWithDisposition guards only
// terminal + pending/submitted, and then transitions the Edge row to
// in_transit locally. So releasing an order that is queued / sourcing /
// dispatched / acknowledged queues an envelope Core will refuse AND moves the
// Edge row anyway — a persistent Edge/Core status divergence plus a bogus
// "released" count. Ask this first and skip instead.
//
// Deliberately status-only. Core resolves the order by UUID, not by Edge's
// mirrored WaybillID, and enforces its own dispatch precondition; requiring a
// non-nil waybill here would add false negatives (a staged leg whose waybill
// write lagged would be skipped and the operator would have to click twice)
// without removing any real failure. Staged/in_transit already implies
// dispatched.
//
// Faulted is intentionally NOT releasable: Core no-ops a faulted release
// rather than erroring, so skipping it costs nothing and saves a round trip.
func ReleasableAtCore(status protocol.Status) bool {
	return status == StatusStaged || status == StatusInTransit
}
