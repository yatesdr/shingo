package dispatch

import "shingo/protocol"

// Order types — aliased to the canonical typed constants in protocol so
// edge and core agree on the wire shape and Go callers get compile-time
// distinction from raw strings.
const (
	OrderTypeRetrieve = protocol.OrderTypeRetrieve
	OrderTypeMove     = protocol.OrderTypeMove
	OrderTypeStore    = protocol.OrderTypeStore
	OrderTypeComplex  = protocol.OrderTypeComplex
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
	StatusReshuffling  = protocol.StatusReshuffling
)

// VendorIDPrefix is prepended to fleet transport order IDs for traceability.
const VendorIDPrefix = "sg-"
