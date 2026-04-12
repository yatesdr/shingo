package engine

import (
	"shingo/protocol/eventbus"
)

// EventBus provides synchronous, typed event dispatch.
type EventBus = eventbus.Bus[EventType]

// SubscriberID uniquely identifies an EventBus subscriber.
type SubscriberID = eventbus.SubscriberID

// SubscriberFunc is a callback invoked when an event is emitted.
type SubscriberFunc = eventbus.SubscriberFunc[EventType]

// NewEventBus creates a new EventBus with panic recovery.
func NewEventBus() *EventBus {
	return eventbus.New[EventType]()
}
