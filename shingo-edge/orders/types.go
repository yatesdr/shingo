package orders

import (
	"shingo/protocol"
)

// Order types — aliased to the canonical typed constants in protocol so
// edge and core agree on the wire shape and Go callers get compile-time
// distinction from raw strings.
const (
	TypeRetrieve = protocol.OrderTypeRetrieve
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

// IsTerminalSuccess reports whether a terminal order RAN ITS HALF — as opposed
// to dying (failed/cancelled) or being found unnecessary (skipped).
//
// THE DISTINCTION IS THE WHOLE SWAP-ORPHAN FIX, and it did not exist because
// nothing needed it. Every arm that reacts to a swap peer going terminal reacts
// to a DEATH: HandleSwapPeerTerminal unwinds the survivor, and Core's
// swapTerminalKind maps skipped/failed/cancelled and deliberately returns "" for
// confirmed. That asymmetry is correct as far as it goes — the unwind exists to
// clean up after a death, and a completed peer did its half. What it leaves
// uncovered is the survivor of a SUCCESSFUL half-swap, which needs no unwind and
// does need a release.
//
// MEASURED, run 12d (order 84 / peer 85, 2026-08-31). The legs were created
// 0.754s apart — a normal paired mint, not an intake defect — and 85 ran its
// whole leg and confirmed at 23:55:20. 84 was released past its first wait at
// 23:55:21 and re-staged at its SECOND wait 58 seconds later, by which time its
// partner had been terminal for a minute. Nothing fires on a peer's SUCCESS, so
// nothing looked at it again.
//
// Confirmed is the only member. Skipped is deliberately excluded even though it
// is not a failure: a skipped leg is one Core found MOOT, and its partner is
// Core's to decide in HandleSwapPeerTerminal, not a survivor this unwind
// releases. Core SPARES the partner of a skipped removal (the line's bin was
// already gone, so the supply runs alone) and cancels the partner of a skipped
// supply. A bare line no longer makes such a pair from the Edge's own requests:
// it gets a plain delivery instead. Delivered is not here because it is not
// terminal at all.
func IsTerminalSuccess(status protocol.Status) bool {
	return status == StatusConfirmed
}

// WaitKindStation mirrors Core's dispatch.WaitKindStation: the wait kind a
// station wait carries, which Core's release fence and population partition
// read. Derived from protocol.WaitKindStation, the one definition;
// engine/material_orders.go derives its constant from this.
const WaitKindStation = protocol.WaitKindStation
