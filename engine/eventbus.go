package engine

import (
	"sync"
	"time"
)

type EventType int

type SubscriberID int

type Event struct {
	Type      EventType
	Timestamp time.Time
	Payload   any
}

type subscriber struct {
	id     SubscriberID
	fn     func(Event)
	filter map[EventType]struct{}
}

type EventBus struct {
	mu          sync.RWMutex
	subscribers []subscriber
	nextID      SubscriberID
}

func NewEventBus() *EventBus {
	return &EventBus{}
}

// Subscribe registers a handler for all event types.
func (eb *EventBus) Subscribe(fn func(Event)) SubscriberID {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.nextID++
	eb.subscribers = append(eb.subscribers, subscriber{id: eb.nextID, fn: fn})
	return eb.nextID
}

// SubscribeTypes registers a handler for specific event types.
func (eb *EventBus) SubscribeTypes(fn func(Event), types ...EventType) SubscriberID {
	filter := make(map[EventType]struct{}, len(types))
	for _, t := range types {
		filter[t] = struct{}{}
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.nextID++
	eb.subscribers = append(eb.subscribers, subscriber{id: eb.nextID, fn: fn, filter: filter})
	return eb.nextID
}

// Unsubscribe removes a subscriber by ID.
func (eb *EventBus) Unsubscribe(id SubscriberID) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	for i, s := range eb.subscribers {
		if s.id == id {
			eb.subscribers = append(eb.subscribers[:i], eb.subscribers[i+1:]...)
			return
		}
	}
}

// Emit sends an event to all matching subscribers.
func (eb *EventBus) Emit(evt Event) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	eb.mu.RLock()
	subs := make([]subscriber, len(eb.subscribers))
	copy(subs, eb.subscribers)
	eb.mu.RUnlock()

	for _, s := range subs {
		if s.filter != nil {
			if _, ok := s.filter[evt.Type]; !ok {
				continue
			}
		}
		s.fn(evt)
	}
}
