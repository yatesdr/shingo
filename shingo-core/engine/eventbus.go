package engine

import (
	"shingo/protocol/eventbus"
)

// EventType identifies a category of event.
type EventType int

// Event carries a typed payload through the EventBus.
type Event = eventbus.Event[EventType]

// SubscriberID uniquely identifies an EventBus subscriber.
type SubscriberID = eventbus.SubscriberID

// SubscriberFunc is a callback invoked when an event is emitted.
type SubscriberFunc = eventbus.SubscriberFunc[EventType]

// EventBus provides synchronous, typed event dispatch.
type EventBus = eventbus.Bus[EventType]

// NewEventBus creates a new EventBus with panic recovery.
func NewEventBus() *EventBus {
	return eventbus.New[EventType]()
}
