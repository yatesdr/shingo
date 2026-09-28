package rds

import "shingo/protocol"

// Response is the common RDS API response envelope.
type Response struct {
	Code     int    `json:"code"`
	Msg      string `json:"msg"`
	CreateOn string `json:"create_on"`
}

// OrderState represents RDS order lifecycle states.
type OrderState string

const (
	StateCreated        OrderState = "CREATED"
	StateToBeDispatched OrderState = "TOBEDISPATCHED"
	StateRunning        OrderState = "RUNNING"
	StateFinished       OrderState = "FINISHED"
	StateFailed         OrderState = "FAILED"
	StateStopped        OrderState = "STOPPED"
	StateWaiting        OrderState = "WAITING"
)

// IsTerminal reports whether the vendor is done with the order in the sense
// Core's dispatch state machine keys on: FINISHED and STOPPED. FAILED is NOT
// terminal here — it maps to Core's faulted, a non-terminal grace state with a
// recovery timer. (Callers asking "does the FLEET still have work on this" add
// FAILED themselves: the simulator's eviction, the chapter floor.)
func (s OrderState) IsTerminal() bool {
	return s == StateFinished || s == StateStopped
}

// CoreStatus is the ONE vendor→Core status map (CW#25). The SEER adapter and
// the simulator both read it; each keeps its own answer for a state it does
// not recognise, which is why this reports ok=false instead of choosing one.
func (s OrderState) CoreStatus() (protocol.Status, bool) {
	switch s {
	case StateCreated, StateToBeDispatched:
		return protocol.StatusDispatched, true
	case StateRunning:
		return protocol.StatusInTransit, true
	case StateWaiting:
		return protocol.StatusStaged, true
	case StateFinished:
		return protocol.StatusDelivered, true
	case StateFailed:
		return protocol.StatusFaulted, true
	case StateStopped:
		return protocol.StatusCancelled, true
	}
	return "", false
}
