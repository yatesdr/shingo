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
)

var validTransitions = map[string][]string{
	StatusPending:      {StatusSubmitted, StatusCancelled, StatusFailed},
	StatusSubmitted:    {StatusAcknowledged, StatusCancelled, StatusFailed},
	StatusAcknowledged: {StatusInTransit, StatusCancelled, StatusFailed},
	StatusInTransit:    {StatusDelivered, StatusStaged, StatusCancelled, StatusFailed},
	StatusStaged:       {StatusInTransit, StatusCancelled, StatusFailed},
	StatusDelivered:    {StatusConfirmed, StatusCancelled, StatusFailed},
}

func IsValidTransition(from, to string) bool {
	allowed, ok := validTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

func IsTerminal(status string) bool {
	return status == StatusConfirmed || status == StatusCancelled || status == StatusFailed
}
