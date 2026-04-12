package eventbus

import (
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// Event is a generic event envelope dispatched through the bus.
// The Type field is an opaque comparable value defined by the consuming package.
type Event[T comparable] struct {
	Type      T
	Timestamp time.Time
	Payload   any
}

// SubscriberID uniquely identifies a subscriber.
type SubscriberID uint64

// SubscriberFunc is a callback invoked when an event is emitted.
type SubscriberFunc[T comparable] func(Event[T])

type subscriber[T comparable] struct {
	id     SubscriberID
	fn     SubscriberFunc[T]
	filter map[T]struct{}
}

// Bus provides synchronous, typed event dispatch.
// Subscribers are called in registration order on the emitting goroutine.
// Panics in subscribers are recovered and logged, preventing one misbehaving
// subscriber from crashing the process.
type Bus[T comparable] struct {
	mu          sync.RWMutex
	subscribers []subscriber[T]
	nextID      SubscriberID
}

// New creates a new EventBus.
func New[T comparable]() *Bus[T] {
	return &Bus[T]{}
}

// Subscribe registers a callback for all event types.
func (eb *Bus[T]) Subscribe(fn SubscriberFunc[T]) SubscriberID {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.nextID++
	id := eb.nextID
	eb.subscribers = append(eb.subscribers, subscriber[T]{id: id, fn: fn})
	return id
}

// SubscribeTypes registers a callback only for the given event types.
func (eb *Bus[T]) SubscribeTypes(fn SubscriberFunc[T], types ...T) SubscriberID {
	filter := make(map[T]struct{}, len(types))
	for _, t := range types {
		filter[t] = struct{}{}
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.nextID++
	id := eb.nextID
	eb.subscribers = append(eb.subscribers, subscriber[T]{id: id, fn: fn, filter: filter})
	return id
}

// Unsubscribe removes a subscriber by ID.
func (eb *Bus[T]) Unsubscribe(id SubscriberID) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	for i, s := range eb.subscribers {
		if s.id == id {
			eb.subscribers = append(eb.subscribers[:i], eb.subscribers[i+1:]...)
			return
		}
	}
}

// Emit dispatches an event synchronously to all matching subscribers.
func (eb *Bus[T]) Emit(evt Event[T]) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	eb.mu.RLock()
	subs := make([]subscriber[T], len(eb.subscribers))
	copy(subs, eb.subscribers)
	eb.mu.RUnlock()

	for _, s := range subs {
		if s.filter != nil {
			if _, ok := s.filter[evt.Type]; !ok {
				continue
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("eventbus: subscriber %d panicked on event type %v: %v\n%s", s.id, evt.Type, r, debug.Stack())
				}
			}()
			s.fn(evt)
		}()
	}
}
