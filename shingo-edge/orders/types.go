package orders

import "shingo/protocol"

// Order types
const (
	TypeRetrieve = "retrieve"
	TypeStore    = "store"
	TypeMove     = "move"
	TypeComplex  = "complex"
	TypeIngest   = "ingest"
)

// Order statuses aliased from protocol.
const (
	StatusPending      = protocol.StatusPending
	StatusQueued       = protocol.StatusQueued
	StatusSubmitted    = protocol.StatusSubmitted
	StatusAcknowledged = protocol.StatusAcknowledged
	StatusInTransit    = protocol.StatusInTransit
	StatusStaged       = protocol.StatusStaged
	StatusDelivered    = protocol.StatusDelivered
	StatusConfirmed    = protocol.StatusConfirmed
	StatusCancelled    = protocol.StatusCancelled
	StatusFailed       = protocol.StatusFailed
)

// Dispatch reply types — used by HandleDispatchReply and edge_handler.
const (
	ReplyAck       = "ack"
	ReplyWaybill   = "waybill"
	ReplyUpdate    = "update"
	ReplyDelivered = "delivered"
	ReplyError     = "error"
	ReplyStaged    = "staged"
	ReplyCancelled = "cancelled"
	ReplyQueued    = "queued"
)

// IsValidTransition delegates to the canonical state machine in protocol.
func IsValidTransition(from, to string) bool {
	return protocol.IsValidTransition(from, to)
}

// IsTerminal delegates to the canonical definition in protocol.
func IsTerminal(status string) bool {
	return protocol.IsTerminal(status)
}
