package messaging

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"shingocore/config"
)

// orderRecordingReader is a kafkaReader that serves a fixed list of messages,
// records every call readLoop makes on it, and then blocks until released.
//
// ReadMessage is recorded as "read+commit" because that is what it is on a
// consumer-group reader: kafka-go v0.4.50 reader.go ReadMessage calls
// FetchMessage and then, when a GroupID is set (Core always sets one),
// CommitMessages before it returns the message.
type orderRecordingReader struct {
	mu      sync.Mutex
	events  []string
	pending []kafka.Message
	drained chan struct{} // closed when the list is exhausted
	release chan struct{} // closed by the test to let the blocked call return
	once    sync.Once
}

func newOrderRecordingReader(msgs ...kafka.Message) *orderRecordingReader {
	return &orderRecordingReader{
		pending: msgs,
		drained: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *orderRecordingReader) record(ev string) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *orderRecordingReader) next() (kafka.Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return kafka.Message{}, false
	}
	m := r.pending[0]
	r.pending = r.pending[1:]
	return m, true
}

func (r *orderRecordingReader) ReadMessage(context.Context) (kafka.Message, error) {
	if m, ok := r.next(); ok {
		r.record("read+commit:" + string(m.Value))
		return m, nil
	}
	r.once.Do(func() { close(r.drained) })
	<-r.release
	return kafka.Message{}, errors.New("reader released")
}

func (r *orderRecordingReader) Close() error { return nil }

func (r *orderRecordingReader) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// runReadLoopOver drives readLoop over the fake until the fake is drained,
// then stops the client and lets readLoop exit through its stop branch.
func runReadLoopOver(t *testing.T, r *orderRecordingReader, handler MessageHandler) {
	t.Helper()
	c := NewClient(&config.MessagingConfig{})
	done := make(chan struct{})
	go func() {
		c.readLoop("shingo.orders", r, handler)
		close(done)
	}()
	select {
	case <-r.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("readLoop did not drain the fake reader")
	}
	c.Close()
	close(r.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readLoop did not exit after Close")
	}
}

// TestReadLoop_CommitsBeforeTheHandlerRuns pins the offset order Core's
// readLoop has today: the offset is committed BEFORE the handler runs, because
// readLoop calls ReadMessage, which fetches and commits in one call. A Core
// crash, or a handler that fails (a Postgres blip inside a delta apply), after
// that commit loses the message: the broker has recorded it as consumed and
// nothing redelivers it.
//
// The recorded order is [read+commit:m1, handle:m1, read+commit:m2, handle:m2].
//
// VERIFY-RED: SYNTH-round2 S2 ("Core commits after handling") changes readLoop
// to FetchMessage, then the handler, then CommitMessages. That change inverts
// this pin: the order becomes [fetch:m1, handle:m1, commit:m1, ...], and this
// test's assertion is flipped in the same commit.
func TestReadLoop_CommitsBeforeTheHandlerRuns(t *testing.T) {
	t.Parallel()
	r := newOrderRecordingReader(
		kafka.Message{Topic: "shingo.orders", Value: []byte("m1")},
		kafka.Message{Topic: "shingo.orders", Value: []byte("m2")},
	)
	runReadLoopOver(t, r, func(_ string, payload []byte) {
		r.record("handle:" + string(payload))
	})

	want := []string{"read+commit:m1", "handle:m1", "read+commit:m2", "handle:m2"}
	if got := r.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("readLoop call order = %v, want %v", got, want)
	}
}

// TestReadLoop_HandlerPanicIsRecoveredAndTheLoopMovesOn pins what a failing
// handler does today. MessageHandler returns nothing, so the only failure the
// loop can see is a panic; readLoop recovers it, logs, and reads the next
// message. The panicking message's offset was already committed by
// ReadMessage, so it is never seen again.
//
// S2 keeps "move on" (a poison message must not wedge the partition) but moves
// the commit after the handler; the panic case then commits after the recover.
func TestReadLoop_HandlerPanicIsRecoveredAndTheLoopMovesOn(t *testing.T) {
	t.Parallel()
	r := newOrderRecordingReader(
		kafka.Message{Topic: "shingo.orders", Value: []byte("poison")},
		kafka.Message{Topic: "shingo.orders", Value: []byte("m2")},
	)
	runReadLoopOver(t, r, func(_ string, payload []byte) {
		r.record("handle:" + string(payload))
		if string(payload) == "poison" {
			panic("handler failure under test")
		}
	})

	want := []string{"read+commit:poison", "handle:poison", "read+commit:m2", "handle:m2"}
	if got := r.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("readLoop call order = %v, want %v", got, want)
	}
}
