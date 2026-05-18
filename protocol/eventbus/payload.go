package eventbus

import (
	"log"
	"time"
)

// Payload is the sealed interface that every typed event payload must
// satisfy. Implementations are sealed by an unexported method, so external
// packages cannot directly implement Payload — they must embed PayloadBase
// to opt in. The seal exists so the compiler can rule out "any value can
// be a payload" at the SubscribeTyped / EmitTyped call sites.
//
// Layering note: the seal is intentional friction. Forcing payload types
// to declare themselves (via PayloadBase embed) catches the class of bug
// where an arbitrary struct is accidentally passed as a payload. Without
// the seal, the generic constraint `P any` would allow anything.
type Payload interface {
	isPayload()
}

// PayloadBase is the embeddable marker that satisfies Payload. Event
// structs that participate in the typed event API embed it as a zero-cost
// (zero-size) marker:
//
//	type OrderDispatchedEvent struct {
//		eventbus.PayloadBase
//		OrderID int64
//		// ...
//	}
type PayloadBase struct{}

func (PayloadBase) isPayload() {}

// TypedEvent is the strongly-typed event envelope. Payload is the concrete
// generic P (a Payload) rather than the `any` field on Event.
type TypedEvent[T comparable, P Payload] struct {
	Type      T
	Timestamp time.Time
	Payload   P
}

// TypedSubscriberFunc is a callback that receives TypedEvents.
type TypedSubscriberFunc[T comparable, P Payload] func(TypedEvent[T, P])

// SubscribeTyped registers a subscriber for one or more event types that
// all carry payloads of the same concrete type P. The wrapped subscriber
// does the runtime type assertion; if a published event's payload isn't
// of type P (mis-emission from somewhere else) the subscriber is skipped
// for that event and a warning is logged so the mismatch is observable.
// The seal blocks most accidents at compile time, but any caller using
// the legacy Bus.Emit with an arbitrary payload can still bypass it.
//
// The pre-typed wiring code used unchecked type assertions that panicked
// on mismatch (the eventbus's defer-recover captured them with stack
// traces). The log message here preserves the same observability without
// the panic overhead.
//
// Go disallows generic methods on generic receivers, so this is a
// standalone generic function taking a *Bus[T]. Matches the stdlib pattern
// (slices.SortFunc, maps.Keys).
func SubscribeTyped[T comparable, P Payload](
	bus *Bus[T],
	fn TypedSubscriberFunc[T, P],
	types ...T,
) SubscriberID {
	var zero P
	return bus.SubscribeTypes(func(evt Event[T]) {
		p, ok := evt.Payload.(P)
		if !ok {
			log.Printf("eventbus: SubscribeTyped payload mismatch on event type %v: got %T, want %T (subscriber skipped)", evt.Type, evt.Payload, zero)
			return
		}
		fn(TypedEvent[T, P]{
			Type:      evt.Type,
			Timestamp: evt.Timestamp,
			Payload:   p,
		})
	}, types...)
}

// EmitTyped builds an Event from a typed payload and dispatches it through
// the underlying Bus[T].Emit. The payload satisfies Payload via the type
// constraint, so callers can't accidentally pass a non-payload value.
func EmitTyped[T comparable, P Payload](bus *Bus[T], typ T, payload P) {
	bus.Emit(Event[T]{
		Type:    typ,
		Payload: payload,
	})
}
