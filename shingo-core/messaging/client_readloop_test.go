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
// FetchMessage and CommitMessages are recorded separately so the test sees
// where the commit falls relative to the handler.
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

func (r *orderRecordingReader) FetchMessage(context.Context) (kafka.Message, error) {
	if m, ok := r.next(); ok {
		r.record("fetch:" + string(m.Value))
		return m, nil
	}
	r.once.Do(func() { close(r.drained) })
	<-r.release
	return kafka.Message{}, errors.New("reader released")
}

func (r *orderRecordingReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	for _, m := range msgs {
		r.record("commit:" + string(m.Value))
	}
	return nil
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

// TestReadLoop_CommitsAfterTheHandlerRuns holds SYNTH-round2 S2 ("Core commits
// after handling"): readLoop fetches, runs the handler, then commits. It used
// to call ReadMessage, which on a consumer-group reader fetches AND commits
// before returning (kafka-go v0.4.50 reader.go ReadMessage), so a Core crash
// inside the handler lost the message. Now the offset moves only after the
// handler has run, and a crash between the two delivers the message once more.
//
// The recorded order is [fetch:m1, handle:m1, commit:m1, fetch:m2, ...].
// Before S2 it was [read+commit:m1, handle:m1, read+commit:m2, handle:m2].
func TestReadLoop_CommitsAfterTheHandlerRuns(t *testing.T) {
	t.Parallel()
	r := newOrderRecordingReader(
		kafka.Message{Topic: "shingo.orders", Value: []byte("m1")},
		kafka.Message{Topic: "shingo.orders", Value: []byte("m2")},
	)
	runReadLoopOver(t, r, func(_ string, payload []byte) {
		r.record("handle:" + string(payload))
	})

	want := []string{"fetch:m1", "handle:m1", "commit:m1", "fetch:m2", "handle:m2", "commit:m2"}
	if got := r.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("readLoop call order = %v, want %v", got, want)
	}
}

// TestReadLoop_HandlerPanicIsRecoveredAndTheLoopMovesOn: MessageHandler
// returns nothing, so the only failure the loop can see is a panic. readLoop
// recovers it, commits the message anyway, and reads the next one. Committing
// after a panic is deliberate: a message that panics every time must not wedge
// the partition.
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

	want := []string{"fetch:poison", "handle:poison", "commit:poison", "fetch:m2", "handle:m2", "commit:m2"}
	if got := r.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("readLoop call order = %v, want %v", got, want)
	}
}

// TestClose_AWedgedHandlerHoldsCloseNoLongerThanItsLimit: Close waits for the
// message in hand (docket B11, pinned by
// TestCoreHandler_AnOrderCutInHalfIsCreatedOnceAfterRedelivery), and only so
// long. A handler that never returns would otherwise hold Core's shutdown
// until the service manager killed it; at closeWait Close goes on and closes
// under the handler.
func TestClose_AWedgedHandlerHoldsCloseNoLongerThanItsLimit(t *testing.T) {
	t.Parallel()
	r := newOrderRecordingReader(kafka.Message{Topic: "shingo.orders", Value: []byte("wedged")})
	c := NewClient(&config.MessagingConfig{})
	c.closeWait = 50 * time.Millisecond
	inHandler, unwedge, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		c.readLoop("shingo.orders", r, func(string, []byte) {
			close(inHandler)
			<-unwedge
		})
		close(done)
	}()
	select {
	case <-inHandler:
	case <-time.After(5 * time.Second):
		t.Fatal("readLoop never handed the message to its handler")
	}

	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close still waited on a wedged handler 5 s into a 50 ms limit")
	}

	close(unwedge)
	close(r.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readLoop did not exit once its handler returned")
	}
}
