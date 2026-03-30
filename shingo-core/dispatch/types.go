package dispatch

import "shingo/protocol"

const (
	OrderTypeRetrieve = "retrieve"
	OrderTypeMove     = "move"
	OrderTypeStore    = "store"
	OrderTypeComplex  = "complex"
)

// Order statuses aliased from protocol for local use.
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
	StatusFailed       = protocol.StatusFailed
	StatusCancelled    = protocol.StatusCancelled
)
