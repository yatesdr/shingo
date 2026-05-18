package eventbus_test

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"shingo/protocol/eventbus"
)

// fakeEventType is the discriminator type used by these tests.
type fakeEventType string

const (
	evtAdded   fakeEventType = "added"
	evtRemoved fakeEventType = "removed"
)

type addedPayload struct {
	eventbus.PayloadBase
	ID   int64
	Name string
}

type removedPayload struct {
	eventbus.PayloadBase
	ID int64
}

// TestSubscribeTyped_DeliversTypedPayloadToSubscriber pins the happy path:
// a payload emitted through EmitTyped arrives at a SubscribeTyped subscriber
// as the concrete struct type (no type assertion needed in user code).
func TestSubscribeTyped_DeliversTypedPayloadToSubscriber(t *testing.T) {
	bus := eventbus.New[fakeEventType]()

	var got addedPayload
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[fakeEventType, addedPayload]) {
		got = evt.Payload
	}, evtAdded)

	eventbus.EmitTyped(bus, evtAdded, addedPayload{ID: 42, Name: "test"})

	if got.ID != 42 || got.Name != "test" {
		t.Errorf("payload not delivered: got %+v, want {ID:42, Name:test}", got)
	}
}

// TestSubscribeTyped_FilterByEventType ensures the type filter still works:
// a subscriber listening on evtAdded does not see evtRemoved emissions.
func TestSubscribeTyped_FilterByEventType(t *testing.T) {
	bus := eventbus.New[fakeEventType]()

	var addedCalls int
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[fakeEventType, addedPayload]) {
		addedCalls++
	}, evtAdded)

	eventbus.EmitTyped(bus, evtAdded, addedPayload{ID: 1})
	eventbus.EmitTyped(bus, evtRemoved, removedPayload{ID: 2})
	eventbus.EmitTyped(bus, evtAdded, addedPayload{ID: 3})

	if addedCalls != 2 {
		t.Errorf("addedCalls = %d, want 2", addedCalls)
	}
}

// TestSubscribeTyped_PayloadTypeMismatchSkippedAndLogged pins the
// defensive behavior on mis-emission: if some other site emits an event
// with the right Type but the wrong payload type, the typed subscriber's
// wrapper skips it (no panic) and logs the mismatch so the bug is
// observable. The wrapper is the seal-bypass test — a non-Payload value
// cannot reach EmitTyped, but Bus.Emit takes any.
//
// The log assertion guards against the silent-loss regression: pre-fix
// the wrapper returned without logging, which is strictly quieter than
// the unchecked type-assertion-panic pattern the original wiring code
// used (eventbus's defer-recover logged those with stack traces).
func TestSubscribeTyped_PayloadTypeMismatchSkippedAndLogged(t *testing.T) {
	bus := eventbus.New[fakeEventType]()

	var addedCalls int
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[fakeEventType, addedPayload]) {
		addedCalls++
	}, evtAdded)

	// Capture log output to verify the mismatch is reported.
	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	})

	// Right event type, wrong payload (something legacy/mis-emitted).
	bus.Emit(eventbus.Event[fakeEventType]{Type: evtAdded, Payload: removedPayload{ID: 99}})
	if addedCalls != 0 {
		t.Errorf("mis-typed payload should not invoke subscriber; got %d calls", addedCalls)
	}
	if !strings.Contains(buf.String(), "SubscribeTyped payload mismatch") {
		t.Errorf("expected mismatch log; got %q", buf.String())
	}

	// Right event type, right payload — should still work.
	buf.Reset()
	eventbus.EmitTyped(bus, evtAdded, addedPayload{ID: 1})
	if addedCalls != 1 {
		t.Errorf("correctly-typed payload should invoke subscriber; got %d calls", addedCalls)
	}
	if buf.Len() != 0 {
		t.Errorf("matched payload should not log; got %q", buf.String())
	}
}
