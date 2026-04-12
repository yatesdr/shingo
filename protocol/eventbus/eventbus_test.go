package eventbus

import (
	"sync/atomic"
	"testing"
)

type testEventType int

const (
	testEventA testEventType = iota + 1
	testEventB
)

func TestNew(t *testing.T) {
	bus := New[testEventType]()
	if bus == nil {
		t.Fatal("New returned nil")
	}
}

func TestSubscribe_Emit(t *testing.T) {
	bus := New[testEventType]()
	var received atomic.Int32

	bus.Subscribe(func(e Event[testEventType]) {
		if e.Type != testEventA {
			t.Errorf("got type %v, want A", e.Type)
		}
		received.Add(1)
	})

	bus.Emit(Event[testEventType]{Type: testEventA, Payload: "hello"})

	if received.Load() != 1 {
		t.Errorf("received %d calls, want 1", received.Load())
	}
}

func TestSubscribeTypes_Filter(t *testing.T) {
	bus := New[testEventType]()
	var countA, countB atomic.Int32

	bus.SubscribeTypes(func(e Event[testEventType]) {
		countA.Add(1)
	}, testEventA)

	bus.SubscribeTypes(func(e Event[testEventType]) {
		countB.Add(1)
	}, testEventB)

	bus.Emit(Event[testEventType]{Type: testEventA})
	bus.Emit(Event[testEventType]{Type: testEventB})
	bus.Emit(Event[testEventType]{Type: testEventA})

	if countA.Load() != 2 {
		t.Errorf("A subscriber called %d times, want 2", countA.Load())
	}
	if countB.Load() != 1 {
		t.Errorf("B subscriber called %d times, want 1", countB.Load())
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := New[testEventType]()
	var count atomic.Int32

	id := bus.Subscribe(func(e Event[testEventType]) {
		count.Add(1)
	})

	bus.Emit(Event[testEventType]{Type: testEventA})
	bus.Unsubscribe(id)
	bus.Emit(Event[testEventType]{Type: testEventA})

	if count.Load() != 1 {
		t.Errorf("called %d times, want 1 (unsubscribed after first)", count.Load())
	}
}

func TestEmit_AutoTimestamp(t *testing.T) {
	bus := New[testEventType]()
	var ts int64

	bus.Subscribe(func(e Event[testEventType]) {
		ts = e.Timestamp.Unix()
	})

	bus.Emit(Event[testEventType]{Type: testEventA})

	if ts == 0 {
		t.Error("timestamp not set")
	}
}

func TestEmit_PanicRecovery(t *testing.T) {
	bus := New[testEventType]()
	var afterPanic atomic.Int32

	// First subscriber panics
	bus.Subscribe(func(e Event[testEventType]) {
		panic("test panic")
	})

	// Second subscriber should still be called
	bus.Subscribe(func(e Event[testEventType]) {
		afterPanic.Add(1)
	})

	// Should not panic
	bus.Emit(Event[testEventType]{Type: testEventA})

	if afterPanic.Load() != 1 {
		t.Errorf("subscriber after panic called %d times, want 1", afterPanic.Load())
	}
}

func TestEmit_MultipleSubscribers(t *testing.T) {
	bus := New[testEventType]()
	var count atomic.Int32

	for i := 0; i < 5; i++ {
		bus.Subscribe(func(e Event[testEventType]) {
			count.Add(1)
		})
	}

	bus.Emit(Event[testEventType]{Type: testEventA})

	if count.Load() != 5 {
		t.Errorf("total calls = %d, want 5", count.Load())
	}
}
