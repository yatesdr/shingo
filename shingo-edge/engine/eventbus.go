package engine

import (
	"shingo/protocol/eventbus"
)

// EventBus provides synchronous, typed event dispatch.
type EventBus = eventbus.Bus[EventType]

// NewEventBus creates a new EventBus with panic recovery.
func NewEventBus() *EventBus {
	return eventbus.New[EventType]()
}
